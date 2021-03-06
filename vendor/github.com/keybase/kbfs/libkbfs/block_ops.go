// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"github.com/keybase/kbfs/kbfsblock"
	"github.com/keybase/kbfs/kbfscodec"
	"github.com/keybase/kbfs/kbfscrypto"
	"github.com/keybase/kbfs/tlf"
	"golang.org/x/net/context"
)

type blockOpsConfig interface {
	dataVersioner
	logMaker
	blockServer() BlockServer
	codec() kbfscodec.Codec
	crypto() cryptoPure
	keyGetter() blockKeyGetter
	blockCache() BlockCache
}

type blockOpsConfigAdapter struct {
	Config
}

var _ blockOpsConfig = (*blockOpsConfigAdapter)(nil)

func (config blockOpsConfigAdapter) blockServer() BlockServer {
	return config.Config.BlockServer()
}

func (config blockOpsConfigAdapter) codec() kbfscodec.Codec {
	return config.Config.Codec()
}

func (config blockOpsConfigAdapter) crypto() cryptoPure {
	return config.Config.Crypto()
}

func (config blockOpsConfigAdapter) keyGetter() blockKeyGetter {
	return config.Config.KeyManager()
}

func (config blockOpsConfigAdapter) blockCache() BlockCache {
	return config.Config.BlockCache()
}

// BlockOpsStandard implements the BlockOps interface by relaying
// requests to the block server.
type BlockOpsStandard struct {
	config  blockOpsConfig
	queue   *blockRetrievalQueue
	workers []*blockRetrievalWorker
}

var _ BlockOps = (*BlockOpsStandard)(nil)

// NewBlockOpsStandard creates a new BlockOpsStandard
func NewBlockOpsStandard(config blockOpsConfig,
	queueSize int) *BlockOpsStandard {
	q := newBlockRetrievalQueue(queueSize, config)
	bops := &BlockOpsStandard{
		config:  config,
		queue:   q,
		workers: make([]*blockRetrievalWorker, 0, queueSize),
	}
	bg := &realBlockGetter{config: config}
	for i := 0; i < queueSize; i++ {
		bops.workers = append(bops.workers, newBlockRetrievalWorker(bg, bops.queue))
	}
	return bops
}

// Get implements the BlockOps interface for BlockOpsStandard.
func (b *BlockOpsStandard) Get(ctx context.Context, kmd KeyMetadata,
	blockPtr BlockPointer, block Block, lifetime BlockCacheLifetime) error {
	errCh := b.queue.Request(ctx, defaultOnDemandRequestPriority, kmd, blockPtr, block, lifetime)
	return <-errCh
}

// Ready implements the BlockOps interface for BlockOpsStandard.
func (b *BlockOpsStandard) Ready(ctx context.Context, kmd KeyMetadata,
	block Block) (id kbfsblock.ID, plainSize int, readyBlockData ReadyBlockData,
	err error) {
	defer func() {
		if err != nil {
			id = kbfsblock.ID{}
			plainSize = 0
			readyBlockData = ReadyBlockData{}
		}
	}()

	crypto := b.config.crypto()

	tlfCryptKey, err := b.config.keyGetter().
		GetTLFCryptKeyForEncryption(ctx, kmd)
	if err != nil {
		return
	}

	// New server key half for the block.
	serverHalf, err := crypto.MakeRandomBlockCryptKeyServerHalf()
	if err != nil {
		return
	}

	blockKey := kbfscrypto.UnmaskBlockCryptKey(serverHalf, tlfCryptKey)
	plainSize, encryptedBlock, err := crypto.EncryptBlock(block, blockKey)
	if err != nil {
		return
	}

	buf, err := b.config.codec().Encode(encryptedBlock)
	if err != nil {
		return
	}

	readyBlockData = ReadyBlockData{
		buf:        buf,
		serverHalf: serverHalf,
	}

	encodedSize := readyBlockData.GetEncodedSize()
	if encodedSize < plainSize {
		err = TooLowByteCountError{
			ExpectedMinByteCount: plainSize,
			ByteCount:            encodedSize,
		}
		return
	}

	id, err = kbfsblock.MakePermanentID(buf)
	if err != nil {
		return
	}

	// Cache the encoded size.
	block.SetEncodedSize(uint32(encodedSize))

	return
}

// Delete implements the BlockOps interface for BlockOpsStandard.
func (b *BlockOpsStandard) Delete(ctx context.Context, tlfID tlf.ID,
	ptrs []BlockPointer) (liveCounts map[kbfsblock.ID]int, err error) {
	contexts := make(kbfsblock.ContextMap)
	for _, ptr := range ptrs {
		contexts[ptr.ID] = append(contexts[ptr.ID], ptr.Context)
	}
	return b.config.blockServer().RemoveBlockReferences(ctx, tlfID, contexts)
}

// Archive implements the BlockOps interface for BlockOpsStandard.
func (b *BlockOpsStandard) Archive(ctx context.Context, tlfID tlf.ID,
	ptrs []BlockPointer) error {
	contexts := make(kbfsblock.ContextMap)
	for _, ptr := range ptrs {
		contexts[ptr.ID] = append(contexts[ptr.ID], ptr.Context)
	}

	return b.config.blockServer().ArchiveBlockReferences(ctx, tlfID, contexts)
}

// TogglePrefetcher implements the BlockOps interface for BlockOpsStandard.
func (b *BlockOpsStandard) TogglePrefetcher(ctx context.Context,
	enable bool) error {
	return b.queue.TogglePrefetcher(ctx, enable)
}

// Prefetcher implements the BlockOps interface for BlockOpsStandard.
func (b *BlockOpsStandard) Prefetcher() Prefetcher {
	return b.queue.Prefetcher()
}

// Shutdown implements the BlockOps interface for BlockOpsStandard.
func (b *BlockOpsStandard) Shutdown() {
	b.queue.Shutdown()
	for _, w := range b.workers {
		w.Shutdown()
	}
}
