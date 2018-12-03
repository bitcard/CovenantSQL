/*
 * Copyright 2018 The CovenantSQL Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package shard

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"

	"github.com/CovenantSQL/CovenantSQL/utils/log"
	"github.com/CovenantSQL/sqlparser"
	"github.com/pkg/errors"
)

const SHARD_SUFFIX = "_ts_"

// buildInsertPlan builds the route for an INSERT statement.
func buildInsertPlan(query string,
	ins *sqlparser.Insert,
	args []driver.NamedValue,
	c *ShardingConn,
) (instructions Primitive, err error) {
	log.Debugf("buildInsertPlan got %s\n insert:%#v\n args: %#v", query, ins, args)
	if conf, ok := c.conf[ins.Table.Name.CompliantName()]; ok {
		if !conf.ShardColName.IsEmpty() && conf.ShardInterval > 0 {
			if ins.Action == sqlparser.ReplaceStr {
				return nil, errors.New("unsupported: REPLACE INTO with sharded schema")
			}
			if ins.OnDup != nil {
				return nil, errors.New("unsupported: OnDup with sharded schema")
			}
			shardColIndex := ins.Columns.FindColumn(conf.ShardColName)
			if shardColIndex < 0 {
				return nil, errors.Errorf("sharding column not found in %s", query)
			} else {
				if rows, ok := ins.Rows.(sqlparser.Values); ok {
					allInserts := &Insert{
						Mutex:        sync.Mutex{},
						Instructions: make([]*SingleRowPrimitive, 0, len(rows)),
					}

					for _, row := range rows {
						if shardColIndex <= len(row)-1 {
							var (
								insertTS  int64
								singleIns *SingleRowPrimitive
								rowArgs   []interface{}
							)
							val := row[shardColIndex]
							if sqlVal, ok := val.(*sqlparser.SQLVal); ok {
								insertTS, err = getShardTS(sqlVal, args)
								if err != nil {
									return nil,
										errors.Wrapf(err, "get shard timestamp failed for: %s", query)
								}
								rowArgs = toValuesNamedArgs(args, row)
								singleIns, err = c.prepareInsertInstruction(insertTS, ins, row, rowArgs, conf)
								//singleIns, err = c.prepareInsertInstruction(insertTS, ins, row, args, conf)
								if err != nil {
									return nil,
										errors.Wrap(err, "prepare shard instruction failed")
								}
								allInserts.Instructions = append(allInserts.Instructions, singleIns)
							} else {
								//TODO(auxten) val can be sqlparser.FuncExpr, process SQL like this:
								// insert into foo(id, name, time) values(61, 'foo', strftime('%s','now'));
								return nil,
									errors.Errorf("non SQLVal type in column is not supported: %s", query)
							}

						} else {
							return nil,
								errors.Errorf("bug: shardColIndex outof range %s", query)
						}
					}
					return allInserts, nil

				} else {
					return nil,
						errors.Errorf("non Values type in Rows is not supported: %s", query)
				}
			}
		} else {
			return nil, errors.Errorf("sharding conf set but not configured: %#v", conf)
		}
	}

	return &BasePrimitive{
		query:   query,
		args:    args,
		rawConn: c.rawConn,
		rawDB:   c.rawDB,
	}, nil
}

type Insert struct {
	sync.Mutex
	Instructions []*SingleRowPrimitive
}

// SingleRowPrimitive is the primitive just insert one row to one shard
type SingleRowPrimitive struct {
	// query is the original query.
	query string
	// namedArgs is the named args
	namedArgs []interface{}
	// rawDB is the userspace sql conn
	rawDB *sql.DB
}

func (s *SingleRowPrimitive) QueryContext(ctx context.Context) (driver.Rows, error) {
	panic("should not call query in insert")
}

func (s *SingleRowPrimitive) ExecContext(ctx context.Context, tx *sql.Tx) (result driver.Result, err error) {
	return tx.ExecContext(ctx, s.query, s.namedArgs...)
}

func (ins *Insert) ExecContext(ctx context.Context, _ *sql.Tx) (result driver.Result, err error) {
	ins.Lock()
	defer ins.Unlock()
	var (
		shardingResult = &ShardingResult{}
		tx             *sql.Tx
		rs             = make([]sql.Result, len(ins.Instructions))
	)

	for i, singleRowPrimitive := range ins.Instructions {
		var (
			r sql.Result
		)
		if i == 0 {
			tx, err = singleRowPrimitive.rawDB.BeginTx(ctx, nil)
			if err != nil {
				err = errors.Wrap(err, "begin tx failed")
				break
			}
		}
		r, err = singleRowPrimitive.ExecContext(ctx, tx)
		if err != nil {
			err = errors.Wrap(err, "execute tx failed")
			break
		}
		rs[i] = r
	}

	// if any error, rollback all
	if err != nil {
		if tx != nil {
			er := tx.Rollback()
			if er != nil {
				err = errors.Wrapf(err, "rollback tx failed: %v", er)
			}
		}
		return
	} else {
		if tx != nil {
			err = tx.Commit()
			if err != nil {
				err = errors.Wrap(err, "commit tx failed")
				return
			}
		}

		for _, r := range rs {
			if r == nil {
				break
			}
			var ra int64
			ra, err = r.RowsAffected()
			if err != nil {
				err = errors.Wrap(err, "get rows affected failed")
				return
			}
			shardingResult.RowsAffectedi += ra
			shardingResult.LastInsertIdi, err = r.LastInsertId()
			if err != nil {
				err = errors.Wrap(err, "get last insert id failed")
				return
			}
		}
		return shardingResult, nil
	}
}

func (ins *Insert) QueryContext(ctx context.Context) (driver.Rows, error) {
	panic("should not call query in insert")
}

func (sc *ShardingConn) prepareShardTable(t *sqlparser.TableName, shardID int64) (err error) {
	sTable := shardTableName(t, shardID)
	if _, ok := sc.shardingTables.Load(sTable); !ok {
		sc.getTableSchema(t.Name.CompliantName())
		var originSchema string
		var shardSchema string
		originSchema, err = sc.getTableSchema(t.Name.CompliantName())
		if err != nil {
			return
		}
		shardSchema, err = generateShardSchema(originSchema, shardID)
		if err == nil {
			_, err = sc.rawDB.Exec(shardSchema)
			if err == nil {
				sc.shardingTables.Store(sTable, true)
				return
			} else {
				return errors.Errorf("creating shard table %s with %s failed: %v",
					sTable, shardSchema, err)
			}
		} else {
			return
		}

	}
	return
}

func (sc *ShardingConn) prepareInsertInstruction(insertTS int64,
	ins *sqlparser.Insert,
	row sqlparser.ValTuple,
	rowArgs []interface{},
	conf *ShardingConf) (p *SingleRowPrimitive, err error) {

	timestampDiff := insertTS - conf.ShardStartTime.Unix()
	if timestampDiff < 0 {
		return nil, errors.Errorf("insert time %d before shard start time", insertTS)
	}
	shardID := timestampDiff / conf.ShardInterval
	log.Debugf("shardID: %d, timestamp diff: %d", shardID, timestampDiff)

	err = sc.prepareShardTable(&ins.Table, shardID)
	if err != nil {
		return nil,
			errors.Wrapf(err, "preparing shard table for %s %d failed",
				ins.Table.Name.CompliantName(), shardID)
	}

	newIns := &sqlparser.Insert{
		Action:   ins.Action,
		Comments: nil,
		Ignore:   ins.Ignore,
		Table: sqlparser.TableName{
			Name:      sqlparser.NewTableIdent(shardTableName(&ins.Table, shardID)),
			Qualifier: sqlparser.NewTableIdent(ins.Table.Qualifier.String()),
		},
		Partitions: ins.Partitions,
		Columns:    ins.Columns,
		Rows:       sqlparser.Values{row},
		OnDup:      nil,
	}
	buf := sqlparser.NewTrackedBuffer(nil)
	newIns.Format(buf)
	p = &SingleRowPrimitive{
		query:     buf.String(),
		namedArgs: rowArgs,
		rawDB:     sc.rawDB,
	}
	log.Debugf("New SQL: %s %v", p.query, p.namedArgs)
	return
}

func toUserSpaceArgs(args []driver.NamedValue) (uArgs []interface{}) {
	uArgs = make([]interface{}, len(args))
	for i, a := range args {
		if a.Name == "" {
			uArgs[i] = a.Value
		} else {
			uArgs[i] = sql.Named(a.Name, a.Value)
		}
	}
	return
}

func toNamedArgs(args []driver.NamedValue) (rowArgs []interface{}) {
	rowArgs = make([]interface{}, len(args))
	for i, a := range args {
		var namedArg sql.NamedArg
		if a.Name == "" {
			namedArg = sql.Named(fmt.Sprintf("v%d", a.Ordinal), a.Value)
		} else {
			namedArg = sql.Named(a.Name, a.Value)
		}
		rowArgs[i] = namedArg
	}
	return
}

func toValuesNamedArgs(args []driver.NamedValue, columns []sqlparser.Expr) (rowArgs []interface{}) {
	rowArgs = make([]interface{}, 0, len(columns))
	for _, col := range columns {
		if colVal, ok := col.(*sqlparser.SQLVal); ok {
			if colVal.Type == sqlparser.ValArg {
				var colArgIndex int64
				if len(colVal.Val) > 1 {
					colName := string(colVal.Val[1:])
					colArgIndex, _ = getValArgIndex(colVal)
					for _, a := range args {
						if a.Name == colName || a.Ordinal == int(colArgIndex) {
							namedArg := sql.Named(colName, a.Value)
							rowArgs = append(rowArgs, namedArg)
						}
					}
				}
			}
		}
	}
	return
}
