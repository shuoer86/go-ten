package components

import (
	"errors"
	"fmt"
	"math/big"

	"golang.org/x/exp/slices"

	"github.com/ethereum/go-ethereum/params"

	"github.com/ten-protocol/go-ten/go/common/errutil"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethlog "github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ten-protocol/go-ten/go/common"
	"github.com/ten-protocol/go-ten/go/common/compression"
	"github.com/ten-protocol/go-ten/go/common/log"
	"github.com/ten-protocol/go-ten/go/enclave/core"
	"github.com/ten-protocol/go-ten/go/enclave/crypto"
	"github.com/ten-protocol/go-ten/go/enclave/storage"
)

/*
RollupCompression - responsible for the compression logic

## Problem
The main overhead (after the tx payloads), are the batch headers.

## Requirements:
1. recreate the exact batch headers as the live ones
2. security - make sure it all chains up cryptographically, so it can't be gamed

## Solution elements:
1. Add another compressed and encrypted metadata blob to the ExtRollup. This is the "CalldataRollupHeader".
It will also be published as calldata, together with the transactions. The role of this header is to contain the bare minimum
information required to recreate the batches.
2. Use implicit position information, deltas, and exceptions to minimise size.
Eg. If the time between 2 batches is always 1second, there is no need to store any extra information.
3. To avoid storing hashes, which don't compress at all, we execute each batch to be able to populate the parent hash.
4. The Signatures over the batches are not stored, since the rollup is itself signed.
5. The cross chain messages are calculated.
*/
type RollupCompression struct {
	dataEncryptionService  crypto.DataEncryptionService
	dataCompressionService compression.DataCompressionService
	batchRegistry          BatchRegistry
	batchExecutor          BatchExecutor
	storage                storage.Storage
	chainConfig            *params.ChainConfig
	logger                 gethlog.Logger
}

func NewRollupCompression(
	batchRegistry BatchRegistry,
	batchExecutor BatchExecutor,
	dataEncryptionService crypto.DataEncryptionService,
	dataCompressionService compression.DataCompressionService,
	storage storage.Storage,
	chainConfig *params.ChainConfig,
	logger gethlog.Logger,
) *RollupCompression {
	return &RollupCompression{
		batchRegistry:          batchRegistry,
		batchExecutor:          batchExecutor,
		dataEncryptionService:  dataEncryptionService,
		dataCompressionService: dataCompressionService,
		storage:                storage,
		chainConfig:            chainConfig,
		logger:                 logger,
	}
}

// temporary data structure to help build a batch from the information found in the rollup
type batchFromRollup struct {
	transactions []*common.L2Tx
	seqNo        *big.Int
	height       *big.Int
	txHash       gethcommon.Hash
	time         uint64
	l1Proof      common.L1BlockHash
	coinbase     gethcommon.Address
	baseFee      *big.Int
	gasLimit     uint64

	header *common.BatchHeader // for reorgs
}

// CreateExtRollup - creates a compressed and encrypted External rollup from the internal data structure
func (rc *RollupCompression) CreateExtRollup(r *core.Rollup) (*common.ExtRollup, error) {
	header, err := rc.createRollupHeader(r)
	if err != nil {
		return nil, err
	}
	encryptedHeader, err := rc.serialiseCompressAndEncrypt(header)
	if err != nil {
		return nil, err
	}

	transactions := make([][]*common.L2Tx, len(r.Batches))
	for i, batch := range r.Batches {
		transactions[i] = batch.Transactions
	}
	encryptedTransactions, err := rc.serialiseCompressAndEncrypt(transactions)
	if err != nil {
		return nil, err
	}

	return &common.ExtRollup{
		Header:               r.Header,
		BatchPayloads:        encryptedTransactions,
		CalldataRollupHeader: encryptedHeader,
	}, nil
}

// ProcessExtRollup - given an External rollup, responsible with checking and saving all batches found inside
func (rc *RollupCompression) ProcessExtRollup(rollup *common.ExtRollup) (*common.CalldataRollupHeader, error) {
	transactionsPerBatch := make([][]*common.L2Tx, 0)
	err := rc.decryptDecompressAndDeserialise(rollup.BatchPayloads, &transactionsPerBatch)
	if err != nil {
		return nil, err
	}

	calldataRollupHeader := new(common.CalldataRollupHeader)
	err = rc.decryptDecompressAndDeserialise(rollup.CalldataRollupHeader, calldataRollupHeader)
	if err != nil {
		return nil, err
	}

	// The recreation of batches is a 2-step process:

	// 1. calculate fields like: sequence, height, time, l1Proof, from the implicit and explicit information from the metadata
	incompleteBatches, err := rc.createIncompleteBatches(calldataRollupHeader, transactionsPerBatch, rollup.Header.CompressionL1Head)
	if err != nil {
		return nil, err
	}

	// 2. execute each batch to be able to calculate the hash which is necessary for the next batch as it is the parent.
	err = rc.executeAndSaveIncompleteBatches(calldataRollupHeader, incompleteBatches)
	if err != nil {
		return nil, err
	}

	return calldataRollupHeader, nil
}

// the main logic that goes from a list of batches to the rollup header
func (rc *RollupCompression) createRollupHeader(rollup *core.Rollup) (*common.CalldataRollupHeader, error) {
	batches := rollup.Batches
	reorgs := make([]*common.BatchHeader, len(batches))

	deltaTimes := make([]*big.Int, len(batches))
	startTime := batches[0].Header.Time
	prev := startTime

	l1HeightDeltas := make([]*big.Int, len(batches))
	var prevL1Height *big.Int

	batchHashes := make([]common.L2BatchHash, len(batches))
	batchHeaders := make([]*common.BatchHeader, len(batches))

	// create an efficient structure to determine whether a batch is canonical
	reorgedBatches, err := rc.storage.FetchNonCanonicalBatchesBetween(batches[0].SeqNo().Uint64(), batches[len(batches)-1].SeqNo().Uint64())
	if err != nil {
		return nil, err
	}
	reorgMap := make(map[uint64]bool)
	for _, batch := range reorgedBatches {
		reorgMap[batch.SeqNo().Uint64()] = true
	}

	for i, batch := range batches {
		rc.logger.Info("Compressing batch to rollup", log.BatchSeqNoKey, batch.SeqNo(), log.BatchHeightKey, batch.Number(), log.BatchHashKey, batch.Hash())
		// determine whether the batch is canonical
		if reorgMap[batch.SeqNo().Uint64()] {
			// if the canonical batch of the same height is different from the current batch
			// then add the entire header to a "reorgs" array
			reorgs[i] = batch.Header
			rc.logger.Info("Reorg", "pos", i)
		} else {
			reorgs[i] = nil
		}
		batchHashes[i] = batch.Hash()
		batchHeaders[i] = batch.Header

		deltaTimes[i] = big.NewInt(int64(batch.Header.Time - prev))
		prev = batch.Header.Time

		// since this is the sequencer, it must have all the blocks, because it created the batches in the first place
		block := rollup.Blocks[batch.Header.L1Proof]

		// the first element is the actual height
		if i == 0 {
			l1HeightDeltas[i] = block.Number()
		} else {
			l1HeightDeltas[i] = big.NewInt(block.Number().Int64() - prevL1Height.Int64())
		}
		prevL1Height = block.Number()
	}

	l1DeltasBA := make([][]byte, len(l1HeightDeltas))
	for i, delta := range l1HeightDeltas {
		v, err := delta.GobEncode()
		if err != nil {
			return nil, err
		}
		l1DeltasBA[i] = v
	}

	timeDeltasBA := make([][]byte, len(deltaTimes))
	for i, delta := range deltaTimes {
		v, err := delta.GobEncode()
		if err != nil {
			return nil, err
		}
		timeDeltasBA[i] = v
	}

	reorgsBA, err := transformToByteArray(reorgs)
	if err != nil {
		return nil, err
	}
	// optimisation in case there is no reorg header
	if len(reorgedBatches) == 0 {
		reorgsBA = nil
	}

	// get the first canonical batch ( which means there is no entry in the reorgs array for it)
	// this is necessary because the height calculations always have to be performed according to what is perceived as a canonical batch.
	firstCanonBatchHeight := batches[0].Number()
	firstCanonParentHash := batches[0].Header.ParentHash
	for i, reorg := range reorgs {
		if reorg == nil {
			firstCanonBatchHeight = batches[i].Number()
			firstCanonParentHash = batches[i].Header.ParentHash
			break
		}
	}

	calldataRollupHeader := &common.CalldataRollupHeader{
		FirstBatchSequence:    batches[0].SeqNo(),
		FirstCanonBatchHeight: firstCanonBatchHeight,
		FirstCanonParentHash:  firstCanonParentHash,
		StartTime:             startTime,
		BatchTimeDeltas:       timeDeltasBA,
		ReOrgs:                reorgsBA,
		L1HeightDeltas:        l1DeltasBA,
		//	BatchHashes:           batchHashes,
		//	BatchHeaders:          batchHeaders,
		Coinbase: batches[0].Header.Coinbase,
		BaseFee:  batches[0].Header.BaseFee,
		GasLimit: batches[0].Header.GasLimit,
	}

	return calldataRollupHeader, nil
}

// the main logic to recreate the batches from the header. The logical pair of: `createRollupHeader`
func (rc *RollupCompression) createIncompleteBatches(calldataRollupHeader *common.CalldataRollupHeader, transactionsPerBatch [][]*common.L2Tx, compressionL1Head common.L1BlockHash) ([]*batchFromRollup, error) {
	incompleteBatches := make([]*batchFromRollup, len(transactionsPerBatch))

	startAtSeq := calldataRollupHeader.FirstBatchSequence.Int64()
	currentHeight := calldataRollupHeader.FirstCanonBatchHeight.Int64() - 1
	currentTime := int64(calldataRollupHeader.StartTime)

	rollupL1Block, err := rc.storage.FetchBlock(compressionL1Head)
	if err != nil {
		return nil, fmt.Errorf("can't find the block used for compression. Cause: %w", err)
	}

	l1Heights, err := rc.calculateL1HeightsFromDeltas(calldataRollupHeader, transactionsPerBatch)
	if err != nil {
		return nil, err
	}

	// a cache of the l1 blocks used by the current rollup, indexed by their height
	l1BlocksAtHeight := make(map[uint64]*types.Block)
	err = rc.calcL1AncestorsOfHeight(big.NewInt(int64(slices.Min(l1Heights))), rollupL1Block, l1BlocksAtHeight)
	if err != nil {
		return nil, err
	}

	for currentBatchIdx, batchTransactions := range transactionsPerBatch {
		// the l1 proofs are stored as deltas, which compress well as it should be a series of 1s and 0s
		// get the block with the currentL1Height, relative to the rollupL1Block
		block, f := l1BlocksAtHeight[l1Heights[currentBatchIdx]]
		if !f {
			return nil, fmt.Errorf("programming error. L1 block not retrieved")
		}

		// todo - this should be 1 second
		// todo - multiply delta by something?
		timeDelta := big.NewInt(0)
		err = timeDelta.GobDecode(calldataRollupHeader.BatchTimeDeltas[currentBatchIdx])
		if err != nil {
			return nil, err
		}
		currentTime += timeDelta.Int64()

		// the transactions stored in a valid rollup belong to sequential batches
		currentSeqNo := big.NewInt(startAtSeq + int64(currentBatchIdx))

		// handle reorgs
		var fullReorgedHeader *common.BatchHeader
		isCanonical := true
		if len(calldataRollupHeader.ReOrgs) > 0 {
			// the ReOrgs data structure contains an entire Header
			// for the batches that got re-orged.
			// the assumption is that it can't be computed because the L1 block won't be available.
			encHeader := calldataRollupHeader.ReOrgs[currentBatchIdx]
			if len(encHeader) > 0 {
				isCanonical = false
				fullReorgedHeader = new(common.BatchHeader)
				err = rlp.DecodeBytes(encHeader, fullReorgedHeader)
				if err != nil {
					return nil, err
				}
			}
		}

		if isCanonical {
			// only if the batch is canonical, increment the height
			currentHeight = currentHeight + 1
		}

		// calculate the hash of the txs
		var txHash gethcommon.Hash
		if len(batchTransactions) == 0 {
			txHash = types.EmptyRootHash
		} else {
			txHash = types.DeriveSha(types.Transactions(batchTransactions), trie.NewStackTrie(nil))
		}

		incompleteBatches[currentBatchIdx] = &batchFromRollup{
			transactions: batchTransactions,
			seqNo:        currentSeqNo,
			height:       big.NewInt(currentHeight),
			txHash:       txHash,
			time:         uint64(currentTime),
			l1Proof:      block.Hash(),
			header:       fullReorgedHeader,
			coinbase:     calldataRollupHeader.Coinbase,
			baseFee:      calldataRollupHeader.BaseFee,
			gasLimit:     calldataRollupHeader.GasLimit,
		}
		rc.logger.Info("Rollup decompressed batch", log.BatchSeqNoKey, currentSeqNo, log.BatchHeightKey, currentHeight, "rollup_idx", currentBatchIdx, "l1_height", block.Number(), "l1_hash", block.Hash())
	}
	return incompleteBatches, nil
}

func (rc *RollupCompression) calculateL1HeightsFromDeltas(calldataRollupHeader *common.CalldataRollupHeader, transactionsPerBatch [][]*common.L2Tx) ([]uint64, error) {
	referenceHeight := big.NewInt(0)
	// the first element in the deltas is the actual height
	err := referenceHeight.GobDecode(calldataRollupHeader.L1HeightDeltas[0])
	if err != nil {
		return nil, err
	}

	l1Heights := make([]uint64, 0)
	l1Heights = append(l1Heights, referenceHeight.Uint64())
	prevHeight := l1Heights[0]
	for currentBatchIdx := range transactionsPerBatch {
		// the l1 proofs are stored as deltas, which compress well as it should be a series of 1s and 0s
		if currentBatchIdx > 0 {
			l1Delta := big.NewInt(0)
			err := l1Delta.GobDecode(calldataRollupHeader.L1HeightDeltas[currentBatchIdx])
			if err != nil {
				return nil, err
			}
			value := l1Delta.Int64() + int64(prevHeight)
			if value < 0 {
				rc.logger.Crit("Should not have a negative height")
			}
			l1Heights = append(l1Heights, uint64(value))
			prevHeight = uint64(value)
		}
	}
	return l1Heights, nil
}

func (rc *RollupCompression) calcL1AncestorsOfHeight(fromHeight *big.Int, toBlock *types.Block, path map[uint64]*types.Block) error {
	path[toBlock.NumberU64()] = toBlock
	if toBlock.NumberU64() == fromHeight.Uint64() {
		return nil
	}
	p, err := rc.storage.FetchBlock(toBlock.ParentHash())
	if err != nil {
		return err
	}
	return rc.calcL1AncestorsOfHeight(fromHeight, p, path)
}

func (rc *RollupCompression) executeAndSaveIncompleteBatches(calldataRollupHeader *common.CalldataRollupHeader, incompleteBatches []*batchFromRollup) error { //nolint:gocognit
	parentHash := calldataRollupHeader.FirstCanonParentHash

	if calldataRollupHeader.FirstBatchSequence.Uint64() != common.L2GenesisSeqNo {
		_, err := rc.storage.FetchBatch(parentHash)
		if err != nil {
			rc.logger.Error("Could not find batch mentioned in the rollup. This should not happen.", log.ErrKey, err)
			return err
		}
	}

	for _, incompleteBatch := range incompleteBatches {
		// check whether the batch is already stored in the database
		b, err := rc.storage.FetchBatchBySeqNo(incompleteBatch.seqNo.Uint64())
		if err == nil {
			// chain to a parent only if the batch is not a reorg
			if incompleteBatch.header == nil {
				parentHash = b.Hash()
			}
			continue
		}
		if !errors.Is(err, errutil.ErrNotFound) {
			return err
		}

		switch {
		// this batch was re-orged
		case incompleteBatch.header != nil:
			err := rc.storage.StoreBatch(&core.Batch{
				Header:       incompleteBatch.header,
				Transactions: incompleteBatch.transactions,
			})
			if err != nil {
				return err
			}

		// handle genesis
		case incompleteBatch.seqNo.Uint64() == common.L2GenesisSeqNo:
			genBatch, _, err := rc.batchExecutor.CreateGenesisState(
				incompleteBatch.l1Proof,
				incompleteBatch.time,
				calldataRollupHeader.Coinbase,
				calldataRollupHeader.BaseFee,
			)
			if err != nil {
				return err
			}
			// Sanity check - uncomment when debugging
			/*if genBatch.Hash() != calldataRollupHeader.BatchHashes[i] {
				rc.logger.Info(fmt.Sprintf("Good %+v \n Calc %+v", calldataRollupHeader.BatchHeaders[i], genBatch.Header))
				rc.logger.Crit("Rollup decompression failure. The check hashes don't match")
			}*/

			err = rc.storage.StoreBatch(genBatch)
			if err != nil {
				return err
			}
			err = rc.storage.StoreExecutedBatch(genBatch, nil)
			if err != nil {
				return err
			}
			rc.batchRegistry.OnBatchExecuted(genBatch, nil)

			rc.logger.Info("Stored genesis", log.BatchHashKey, genBatch.Hash())
			parentHash = genBatch.Hash()

		default:
			// transforms the incompleteBatch into a BatchHeader by executing the transactions
			// and then the info can be used to fill in the parent
			computedBatch, err := rc.computeBatch(incompleteBatch.l1Proof,
				parentHash,
				incompleteBatch.transactions,
				incompleteBatch.time,
				incompleteBatch.seqNo,
				incompleteBatch.coinbase,
				incompleteBatch.baseFee,
			)
			if err != nil {
				return err
			}
			// Sanity check - uncomment when debugging
			/*		if computedBatch.Batch.Hash() != calldataRollupHeader.BatchHashes[i] {
					rc.logger.Info(fmt.Sprintf("Good %+v\nCalc %+v", calldataRollupHeader.BatchHeaders[i], computedBatch.Batch.Header))
					rc.logger.Crit("Rollup decompression failure. The check hashes don't match")
				}*/

			if _, err := computedBatch.Commit(true); err != nil {
				return fmt.Errorf("cannot commit stateDB for incoming valid batch seq=%d. Cause: %w", incompleteBatch.seqNo, err)
			}

			err = rc.storage.StoreBatch(computedBatch.Batch)
			if err != nil {
				return err
			}
			err = rc.storage.StoreExecutedBatch(computedBatch.Batch, computedBatch.Receipts)
			if err != nil {
				return err
			}
			rc.batchRegistry.OnBatchExecuted(computedBatch.Batch, nil)

			parentHash = computedBatch.Batch.Hash()
		}
	}
	return nil
}

func (rc *RollupCompression) serialiseCompressAndEncrypt(obj any) ([]byte, error) {
	serialised, err := rlp.EncodeToBytes(obj)
	if err != nil {
		return nil, err
	}
	compressed, err := rc.dataCompressionService.CompressRollup(serialised)
	if err != nil {
		return nil, err
	}
	encrypted, err := rc.dataEncryptionService.Encrypt(compressed)
	if err != nil {
		return nil, err
	}
	return encrypted, nil
}

func (rc *RollupCompression) decryptDecompressAndDeserialise(blob []byte, obj any) error {
	plaintextBlob, err := rc.dataEncryptionService.Decrypt(blob)
	if err != nil {
		return err
	}
	serialisedBlob, err := rc.dataCompressionService.Decompress(plaintextBlob)
	if err != nil {
		return err
	}
	err = rlp.DecodeBytes(serialisedBlob, obj)
	if err != nil {
		return err
	}
	return nil
}

func (rc *RollupCompression) computeBatch(
	BlockPtr common.L1BlockHash,
	ParentPtr common.L2BatchHash,
	Transactions common.L2Transactions,
	AtTime uint64,
	SequencerNo *big.Int,
	Coinbase gethcommon.Address,
	BaseFee *big.Int,
) (*ComputedBatch, error) {
	return rc.batchExecutor.ComputeBatch(&BatchExecutionContext{
		BlockPtr:     BlockPtr,
		ParentPtr:    ParentPtr,
		Transactions: Transactions,
		AtTime:       AtTime,
		Creator:      Coinbase,
		ChainConfig:  rc.chainConfig,
		SequencerNo:  SequencerNo,
		BaseFee:      big.NewInt(0).Set(BaseFee),
	}, false)
}

func transformToByteArray(reorgs []*common.BatchHeader) ([][]byte, error) {
	reorgsBA := make([][]byte, len(reorgs))
	for i, reorg := range reorgs {
		if reorg != nil {
			enc, err := rlp.EncodeToBytes(reorg)
			if err != nil {
				return nil, err
			}
			reorgsBA[i] = enc
		} else {
			reorgsBA[i] = []byte{}
		}
	}
	return reorgsBA, nil
}
