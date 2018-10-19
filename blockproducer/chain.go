/*
 * Copyright 2018 The CovenantSQL Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package blockproducer

import (
	"fmt"
	"os"
	"sync"
	"time"

	pi "github.com/CovenantSQL/CovenantSQL/blockproducer/interfaces"
	"github.com/CovenantSQL/CovenantSQL/blockproducer/types"
	"github.com/CovenantSQL/CovenantSQL/crypto/asymmetric"
	"github.com/CovenantSQL/CovenantSQL/crypto/hash"
	"github.com/CovenantSQL/CovenantSQL/crypto/kms"
	"github.com/CovenantSQL/CovenantSQL/merkle"
	"github.com/CovenantSQL/CovenantSQL/proto"
	"github.com/CovenantSQL/CovenantSQL/route"
	"github.com/CovenantSQL/CovenantSQL/rpc"
	"github.com/CovenantSQL/CovenantSQL/utils/log"
	"github.com/coreos/bbolt"
)

var (
	metaBucket                     = [4]byte{0x0, 0x0, 0x0, 0x0}
	metaStateKey                   = []byte("covenantsql-state")
	metaBlockIndexBucket           = []byte("covenantsql-block-index-bucket")
	metaTransactionBucket          = []byte("covenantsql-tx-index-bucket")
	metaAccountIndexBucket         = []byte("covenantsql-account-index-bucket")
	metaSQLChainIndexBucket        = []byte("covenantsql-sqlchain-index-bucket")
	gasPrice                uint32 = 1
	accountAddress          proto.AccountAddress
)

// Chain defines the main chain.
type Chain struct {
	db *bolt.DB
	ms *metaState
	bi *blockIndex
	rt *rt
	cl *rpc.Caller

	blocksFromSelf chan *types.Block
	blocksFromRPC  chan *types.Block
	pendingTxs     chan pi.Transaction
	stopCh         chan struct{}
}

// NewChain creates a new blockchain.
func NewChain(cfg *Config) (*Chain, error) {
	if fi, err := os.Stat(cfg.DataFile); err == nil && fi.Mode().IsRegular() {
		return LoadChain(cfg)
	}

	// open db file
	db, err := bolt.Open(cfg.DataFile, 0600, nil)
	if err != nil {
		return nil, err
	}

	// get accountAddress
	pubKey, err := kms.GetLocalPublicKey()
	if err != nil {
		return nil, err
	}
	enc, err := pubKey.MarshalHash()
	if err != nil {
		return nil, err
	}
	accountAddress = proto.AccountAddress(hash.THashH(enc[:]))

	// create bucket for meta data
	err = db.Update(func(tx *bolt.Tx) (err error) {
		bucket, err := tx.CreateBucketIfNotExists(metaBucket[:])

		if err != nil {
			return
		}

		_, err = bucket.CreateBucketIfNotExists(metaBlockIndexBucket)
		if err != nil {
			return
		}

		txbk, err := bucket.CreateBucketIfNotExists(metaTransactionBucket)
		if err != nil {
			return
		}
		for i := pi.TransactionType(0); i < pi.TransactionTypeNumber; i++ {
			if _, err = txbk.CreateBucketIfNotExists(i.Bytes()); err != nil {
				return
			}
		}

		_, err = bucket.CreateBucketIfNotExists(metaAccountIndexBucket)
		if err != nil {
			return
		}

		_, err = bucket.CreateBucketIfNotExists(metaSQLChainIndexBucket)
		return
	})
	if err != nil {
		return nil, err
	}

	// create chain
	chain := &Chain{
		db:             db,
		ms:             newMetaState(),
		bi:             newBlockIndex(),
		rt:             newRuntime(cfg, accountAddress),
		cl:             rpc.NewCaller(),
		blocksFromSelf: make(chan *types.Block),
		blocksFromRPC:  make(chan *types.Block),
		pendingTxs:     make(chan pi.Transaction),
		stopCh:         make(chan struct{}),
	}

	log.Debugf("pushing genesis block: %v", cfg.Genesis)

	if err = chain.pushGenesisBlock(cfg.Genesis); err != nil {
		return nil, err
	}

	log.WithFields(log.Fields{
		"index":     chain.rt.index,
		"bp_number": chain.rt.bpNum,
		"period":    chain.rt.period.String(),
		"tick":      chain.rt.tick.String(),
		"head":      chain.rt.getHead().getHeader().String(),
		"height":    chain.rt.getHead().getHeight(),
	}).Debug("current chain state")

	return chain, nil
}

// LoadChain rebuilds the chain from db.
func LoadChain(cfg *Config) (chain *Chain, err error) {
	// open db file
	db, err := bolt.Open(cfg.DataFile, 0600, nil)
	if err != nil {
		return nil, err
	}

	// get accountAddress
	pubKey, err := kms.GetLocalPublicKey()
	if err != nil {
		return nil, err
	}
	enc, err := pubKey.MarshalHash()
	if err != nil {
		return nil, err
	}
	accountAddress = proto.AccountAddress(hash.THashH(enc[:]))

	chain = &Chain{
		db:             db,
		ms:             newMetaState(),
		bi:             newBlockIndex(),
		rt:             newRuntime(cfg, accountAddress),
		cl:             rpc.NewCaller(),
		blocksFromSelf: make(chan *types.Block),
		blocksFromRPC:  make(chan *types.Block),
		pendingTxs:     make(chan pi.Transaction),
		stopCh:         make(chan struct{}),
	}

	err = chain.db.View(func(tx *bolt.Tx) (err error) {
		meta := tx.Bucket(metaBucket[:])
		state := &State{}
		// TODO(), should test if fetch metaState failed
		if err = state.deserialize(meta.Get(metaStateKey)); err != nil {
			return
		}
		chain.rt.setHead(state)

		var last *blockNode
		var index int32
		blocks := meta.Bucket(metaBlockIndexBucket)
		nodes := make([]blockNode, blocks.Stats().KeyN)

		if err = blocks.ForEach(func(k, v []byte) (err error) {
			block := &types.Block{}

			if err = block.Deserialize(v); err != nil {
				log.Errorf("loadeing block: %v", err)
				return err
			}

			parent := (*blockNode)(nil)

			if last == nil {
				// TODO(lambda): check genesis block
			} else if block.SignedHeader.ParentHash.IsEqual(&last.hash) {
				if err = block.SignedHeader.Verify(); err != nil {
					return err
				}

				parent = last
			} else {
				parent = chain.bi.lookupBlock(block.SignedHeader.BlockHash)

				if parent == nil {
					return ErrParentNotFound
				}
			}

			nodes[index].initBlockNode(block, parent)
			last = &nodes[index]
			index++
			return err
		}); err != nil {
			return err
		}

		// Reload state
		if err = chain.ms.reloadProcedure()(tx); err != nil {
			return
		}

		return
	})
	if err != nil {
		return nil, err
	}

	return chain, nil
}

// checkBlock has following steps: 1. check parent block 2. checkTx 2. merkle tree 3. Hash 4. Signature.
func (c *Chain) checkBlock(b *types.Block) (err error) {
	// TODO(lambda): process block fork
	if !b.SignedHeader.ParentHash.IsEqual(c.rt.getHead().getHeader()) {
		log.WithFields(log.Fields{
			"head":            c.rt.getHead().getHeader().String(),
			"height":          c.rt.getHead().getHeight(),
			"received_parent": b.SignedHeader.ParentHash,
		}).Debug("invalid parent")
		return ErrParentNotMatch
	}

	rootHash := merkle.NewMerkle(b.GetTxHashes()).GetRoot()
	if !b.SignedHeader.MerkleRoot.IsEqual(rootHash) {
		return ErrInvalidMerkleTreeRoot
	}

	enc, err := b.SignedHeader.Header.MarshalHash()
	if err != nil {
		return err
	}
	h := hash.THashH(enc)
	if !b.SignedHeader.BlockHash.IsEqual(&h) {
		return ErrInvalidHash
	}

	return nil
}

func (c *Chain) pushBlockWithoutCheck(b *types.Block) error {
	h := c.rt.getHeightFromTime(b.Timestamp())
	node := newBlockNode(h, b, c.rt.getHead().getNode())
	state := &State{
		Node:   node,
		Head:   node.hash,
		Height: node.height,
	}

	encBlock, err := b.Serialize()
	if err != nil {
		return err
	}

	encState, err := c.rt.getHead().serialize()
	if err != nil {
		return err
	}

	err = c.db.Update(func(tx *bolt.Tx) (err error) {
		err = tx.Bucket(metaBucket[:]).Put(metaStateKey, encState)
		if err != nil {
			return err
		}
		err = tx.Bucket(metaBucket[:]).Bucket(metaBlockIndexBucket).Put(node.indexKey(), encBlock)
		if err != nil {
			return err
		}
		for _, v := range b.Transactions {
			if err = c.ms.applyTransactionProcedure(v)(tx); err != nil {
				return err
			}
		}
		err = c.ms.partialCommitProcedure(b.Transactions)(tx)
		return
	})
	if err != nil {
		return err
	}
	c.rt.setHead(state)
	c.bi.addBlock(node)
	return nil
}

func (c *Chain) pushGenesisBlock(b *types.Block) (err error) {
	err = c.pushBlockWithoutCheck(b)
	if err != nil {
		log.Errorf("push genesis block failed: %v", err)
	}
	return
}

func (c *Chain) pushBlock(b *types.Block) error {
	err := c.checkBlock(b)
	if err != nil {
		return err
	}

	err = c.pushBlockWithoutCheck(b)
	if err != nil {
		return err
	}

	return nil
}

func (c *Chain) produceBlock(now time.Time) error {
	priv, err := kms.GetLocalPrivateKey()
	if err != nil {
		return err
	}

	b := &types.Block{
		SignedHeader: types.SignedHeader{
			Header: types.Header{
				Version:    blockVersion,
				Producer:   c.rt.accountAddress,
				ParentHash: *c.rt.getHead().getHeader(),
				Timestamp:  now,
			},
		},
		Transactions: c.ms.pullTxs(),
	}

	err = b.PackAndSignBlock(priv)
	if err != nil {
		return err
	}

	log.Debugf("generate new block: %v", b)

	err = c.pushBlockWithoutCheck(b)
	if err != nil {
		return err
	}

	peers := c.rt.getPeers()
	wg := &sync.WaitGroup{}
	for _, s := range peers.Servers {
		if !s.ID.IsEqual(&c.rt.nodeID) {
			wg.Add(1)
			go func(id proto.NodeID) {
				defer wg.Done()
				blockReq := &AdviseNewBlockReq{
					Envelope: proto.Envelope{
						// TODO(lambda): Add fields.
					},
					Block: b,
				}
				blockResp := &AdviseNewBlockResp{}
				if err := c.cl.CallNode(id, route.MCCAdviseNewBlock.String(), blockReq, blockResp); err != nil {
					log.WithFields(log.Fields{
						"peer":       c.rt.getPeerInfoString(),
						"curr_turn":  c.rt.getNextTurn(),
						"now_time":   time.Now().UTC().Format(time.RFC3339Nano),
						"block_hash": b.SignedHeader.BlockHash,
					}).WithError(err).Error(
						"Failed to advise new block")
				} else {
					log.Debugf("Success to advising #%d height block to %s", c.rt.getHead().getHeight(), id)
				}
			}(s.ID)
		}
	}

	return err
}

func (c *Chain) produceTxBilling(br *types.BillingRequest) (_ *types.BillingRequest, err error) {
	// TODO(lambda): simplify the function
	if err = c.checkBillingRequest(br); err != nil {
		return
	}

	// update stable coin's balance
	// TODO(lambda): because there is no token distribution,
	// we only increase miners' balance but not decrease customer's balance
	var (
		accountNumber = len(br.Header.GasAmounts)
		receivers     = make([]*proto.AccountAddress, accountNumber)
		fees          = make([]uint64, accountNumber)
		rewards       = make([]uint64, accountNumber)
	)

	for i, addrAndGas := range br.Header.GasAmounts {
		receivers[i] = &addrAndGas.AccountAddress
		fees[i] = addrAndGas.GasAmount * uint64(gasPrice)
		rewards[i] = 0
	}

	// add block producer signature
	var privKey *asymmetric.PrivateKey
	privKey, err = kms.GetLocalPrivateKey()
	if err != nil {
		return
	}

	if _, _, err = br.SignRequestHeader(privKey, false); err != nil {
		return
	}

	// generate and push the txbilling
	// 1. generate txbilling
	var nc pi.AccountNonce
	if nc, err = c.ms.nextNonce(accountAddress); err != nil {
		return
	}
	var (
		tc = types.NewTxContent(nc, br, accountAddress, receivers, fees, rewards)
		tb = types.NewTxBilling(tc)
	)
	if err = tb.Sign(privKey); err != nil {
		return
	}
	log.Debugf("response is %s", br.RequestHash)

	// 2. push tx
	c.pendingTxs <- tb

	return br, nil
}

// checkBillingRequest checks followings by order:
// 1. period of sqlchain;
// 2. request's hash
// 3. miners' signatures.
func (c *Chain) checkBillingRequest(br *types.BillingRequest) (err error) {
	// period of sqlchain;
	// TODO(lambda): get and check period and miner list of specific sqlchain

	err = br.VerifySignatures()
	return
}

func (c *Chain) fetchBlockByHeight(h uint32) (*types.Block, error) {
	node := c.rt.getHead().getNode().ancestor(h)
	if node == nil {
		return nil, ErrNoSuchBlock
	}

	b := &types.Block{}
	k := node.indexKey()

	err := c.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(metaBucket[:]).Bucket(metaBlockIndexBucket).Get(k)
		return b.Deserialize(v)
	})
	if err != nil {
		return nil, err
	}

	return b, nil
}

// runCurrentTurn does the check and runs block producing if its my turn.
func (c *Chain) runCurrentTurn(now time.Time) {
	log.WithFields(log.Fields{
		"next_turn":  c.rt.getNextTurn(),
		"bp_number":  c.rt.bpNum,
		"node_index": c.rt.index,
	}).Info("check turns")
	defer c.rt.setNextTurn()

	if !c.rt.isMyTurn() {
		return
	}

	log.Infof("produce a new block with height %d", c.rt.getNextTurn())
	if err := c.produceBlock(now); err != nil {
		log.WithField("now", now.Format(time.RFC3339Nano)).WithError(err).Errorln(
			"Failed to produce block")
	}
}

// sync synchronizes blocks and queries from the other peers.
func (c *Chain) sync() error {
	log.WithFields(log.Fields{
		"peer": c.rt.getPeerInfoString(),
	}).Debug("Synchronizing chain state")

	for {
		now := c.rt.now()
		height := c.rt.getHeightFromTime(now)

		log.Infof("current height is %d, next turn is %d", height, c.rt.getNextTurn())
		if c.rt.getNextTurn() >= height {
			log.Infof("return with height %d, next turn is %d", height, c.rt.getNextTurn())
			break
		}

		for c.rt.getNextTurn() <= height {
			// TODO(lambda): fetch blocks and txes.
			c.rt.setNextTurn()
			// TODO(lambda): remove it after implementing fetch
			c.rt.getHead().increaseHeightByOne()
		}
	}

	return nil
}

// Start starts the chain by step:
// 1. sync the chain
// 2. goroutine for getting blocks
// 3. goroutine for getting txes.
func (c *Chain) Start() error {
	err := c.sync()
	if err != nil {
		return err
	}

	c.rt.wg.Add(1)
	go c.processBlocks()
	c.rt.wg.Add(1)
	go c.processTxs()
	c.rt.wg.Add(1)
	go c.mainCycle()
	c.rt.startService(c)

	return nil
}

func (c *Chain) processBlocks() {
	rsCh := make(chan struct{})
	rsWG := &sync.WaitGroup{}
	returnStash := func(stash []*types.Block) {
		defer rsWG.Done()
		for _, block := range stash {
			select {
			case c.blocksFromRPC <- block:
			case <-rsCh:
				return
			}
		}
	}

	defer func() {
		close(rsCh)
		rsWG.Wait()
		c.rt.wg.Done()
	}()

	var stash []*types.Block
	for {
		select {
		case block := <-c.blocksFromSelf:
			h := c.rt.getHeightFromTime(block.Timestamp())
			if h == c.rt.getNextTurn()-1 {
				err := c.pushBlockWithoutCheck(block)
				if err != nil {
					log.Error(err)
				}
			}
		case block := <-c.blocksFromRPC:
			if h := c.rt.getHeightFromTime(block.Timestamp()); h > c.rt.getNextTurn()-1 {
				// Stash newer blocks for later check
				if stash == nil {
					stash = make([]*types.Block, 0)
				}
				stash = append(stash, block)
			} else {
				// Process block
				if h < c.rt.getNextTurn()-1 {
					// TODO(lambda): check and add to fork list.
				} else {
					err := c.pushBlock(block)
					if err != nil {
						log.Error(err)
					}
				}

				// Return all stashed blocks to pending channel
				if stash != nil {
					rsWG.Add(1)
					go returnStash(stash)
					stash = nil
				}
			}
		case <-c.stopCh:
			return
		}
	}
}

func (c *Chain) processTx(tx pi.Transaction) (err error) {
	return c.db.Update(c.ms.applyTransactionProcedure(tx))
}

func (c *Chain) processTxs() {
	defer c.rt.wg.Done()
	for {
		select {
		case tx := <-c.pendingTxs:
			if err := c.processTx(tx); err != nil {
				log.WithFields(log.Fields{
					"peer":        c.rt.getPeerInfoString(),
					"next_turn":   c.rt.getNextTurn(),
					"head_height": c.rt.getHead().getHeight(),
					"head_block":  c.rt.getHead().getHeader().String(),
					"transaction": tx.GetHash().String(),
				}).Debugf("Failed to push tx with error: %v", err)
			}
		case <-c.stopCh:
			return
		}
	}
}

func (c *Chain) mainCycle() {
	defer func() {
		c.rt.wg.Done()
		// Signal worker goroutines to stop
		close(c.stopCh)
	}()

	for {
		select {
		case <-c.rt.stopCh:
			return
		default:
			c.syncHead()

			if t, d := c.rt.nextTick(); d > 0 {
				log.WithFields(log.Fields{
					"peer":        c.rt.getPeerInfoString(),
					"next_turn":   c.rt.getNextTurn(),
					"head_height": c.rt.getHead().getHeight(),
					"head_block":  c.rt.getHead().getHeader().String(),
					"now_time":    t.Format(time.RFC3339Nano),
					"duration":    d,
				}).Debug("Main cycle")
				time.Sleep(d)
			} else {
				c.runCurrentTurn(t)
			}
		}
	}
}

func (c *Chain) syncHead() {
	// Try to fetch if the the block of the current turn is not advised yet
	log.WithFields(log.Fields{
		"index":     c.rt.index,
		"next_turn": c.rt.getNextTurn(),
		"height":    c.rt.getHead().getHeight(),
		"count":     c.rt.getHead().getNode().count,
	}).Debugf("sync header")
	if h := c.rt.getNextTurn() - 1; c.rt.getHead().getHeight() < h {
		var err error
		req := &FetchBlockReq{
			Envelope: proto.Envelope{
				// TODO(lambda): Add fields.
			},
			Height: h,
		}
		resp := &FetchBlockResp{}
		peers := c.rt.getPeers()
		succ := false

		for i, s := range peers.Servers {
			if !s.ID.IsEqual(&c.rt.nodeID) {
				err = c.cl.CallNode(s.ID, route.MCCFetchBlock.String(), req, resp)
				if err != nil || resp.Block == nil {
					log.WithFields(log.Fields{
						"peer":        c.rt.getPeerInfoString(),
						"remote":      fmt.Sprintf("[%d/%d] %s", i, len(peers.Servers), s.ID),
						"curr_turn":   c.rt.getNextTurn(),
						"head_height": c.rt.getHead().getHeight(),
						"head_block":  c.rt.getHead().getHeader().String(),
					}).WithError(err).Debug(
						"Failed to fetch block from peer")
				} else {
					c.blocksFromRPC <- resp.Block
					log.WithFields(log.Fields{
						"peer":        c.rt.getPeerInfoString(),
						"remote":      fmt.Sprintf("[%d/%d] %s", i, len(peers.Servers), s.ID),
						"curr_turn":   c.rt.getNextTurn(),
						"head_height": c.rt.getHead().getHeight(),
						"head_block":  c.rt.getHead().getHeader().String(),
					}).Debug(
						"Fetch block from remote peer successfully")
					succ = true
					break
				}
			}
		}

		if !succ {
			log.WithFields(log.Fields{
				"peer":        c.rt.getPeerInfoString(),
				"curr_turn":   c.rt.getNextTurn(),
				"head_height": c.rt.getHead().getHeight(),
				"head_block":  c.rt.getHead().getHeader().String(),
			}).Debug(
				"Cannot get block from any peer")
		}
	}
}

// Stop stops the main process of the sql-chain.
func (c *Chain) Stop() (err error) {
	// Stop main process
	log.WithFields(log.Fields{"peer": c.rt.getPeerInfoString()}).Debug("Stopping chain")
	c.rt.stop()
	log.WithFields(log.Fields{"peer": c.rt.getPeerInfoString()}).Debug("Chain service stopped")
	// Close database file
	err = c.db.Close()
	log.WithFields(log.Fields{"peer": c.rt.getPeerInfoString()}).Debug("Chain database closed")
	return
}
