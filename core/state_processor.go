// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/harmony-one/harmony/block"
	consensus_engine "github.com/harmony-one/harmony/consensus/engine"
	"github.com/harmony-one/harmony/consensus/reward"
	"github.com/harmony-one/harmony/core/state"
	"github.com/harmony-one/harmony/core/types"
	"github.com/harmony-one/harmony/core/vm"
	"github.com/harmony-one/harmony/internal/params"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/shard"
	"github.com/harmony-one/harmony/staking/slash"
	"github.com/pkg/errors"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type StateProcessor struct {
	config *params.ChainConfig     // Chain configuration options
	bc     *BlockChain             // Canonical block chain
	engine consensus_engine.Engine // Consensus engine used for block rewards
}

// NewStateProcessor initialises a new StateProcessor.
func NewStateProcessor(
	config *params.ChainConfig, bc *BlockChain, engine consensus_engine.Engine,
) *StateProcessor {
	return &StateProcessor{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
func (p *StateProcessor) Process(
	block *types.Block, statedb *state.DB, cfg vm.Config,
) (
	types.Receipts, types.CXReceipts,
	[]*types.Log, uint64, reward.Reader, error,
) {
	var (
		receipts types.Receipts
		outcxs   types.CXReceipts
		incxs    = block.IncomingReceipts()
		usedGas  = new(uint64)
		header   = block.Header()
		allLogs  []*types.Log
		gp       = new(GasPool).AddGas(block.GasLimit())
	)
	beneficiary, err := p.bc.GetECDSAFromCoinbase(header)

	if err != nil {
		return nil, nil, nil, 0, nil, err
	}

	// Iterate over and process the individual transactions
	for i, tx := range block.Transactions() {
		statedb.Prepare(tx.Hash(), block.Hash(), i)
		receipt, cxReceipt, _, err := ApplyTransaction(
			p.config, p.bc, &beneficiary, gp, statedb, header, tx, usedGas, cfg,
		)
		if err != nil {
			return nil, nil, nil, 0, nil, err
		}
		receipts = append(receipts, receipt)
		if cxReceipt != nil {
			outcxs = append(outcxs, cxReceipt)
		}
		allLogs = append(allLogs, receipt.Logs...)
	}

	// incomingReceipts should always be processed
	// after transactions (to be consistent with the block proposal)
	for _, cx := range block.IncomingReceipts() {
		if err := ApplyIncomingReceipt(
			p.config, statedb, header, cx,
		); err != nil {
			return nil, nil,
				nil, 0, nil, errors.New("[Process] Cannot apply incoming receipts")
		}
	}

	slashes := slash.Records{}
	if s := header.Slashes(); len(s) > 0 {
		if err := rlp.DecodeBytes(s, &slashes); err != nil {
			return nil, nil, nil, 0, nil, errors.New(
				"[Process] Cannot finalize block",
			)
		}
	}

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	_, payout, err := p.engine.Finalize(
		p.bc, header, statedb, block.Transactions(),
		receipts, outcxs, incxs, slashes,
	)
	if err != nil {
		return nil, nil, nil, 0, nil, errors.New("[Process] Cannot finalize block")
	}

	return receipts, outcxs, allLogs, *usedGas, payout, nil
}

func getTransactionType(
	config *params.ChainConfig, header *block.Header, tx *types.Transaction,
) types.TransactionType {
	if tx.IsStaking() {
		return tx.Type()
	}
	shardID, err := tx.ShardID()
	if err != nil {
		return types.InvalidTx
	}
	toShardID, err := tx.ToShardID()
	if err != nil {
		return types.InvalidTx
	}
	// Same shard or cross-shard before cross-shard epoch treated as same shard
	if header.ShardID() == shardID &&
		(!config.AcceptsCrossTx(header.Epoch()) || shardID == toShardID) {
		return types.SameShardTx
	}
	numShards := shard.Schedule.InstanceForEpoch(header.Epoch()).NumShards()
	// Assuming here all the shards are consecutive from 0 to n-1, n is total number of shards
	if shardID != toShardID &&
		header.ShardID() == shardID &&
		toShardID < numShards {
		return types.SubtractionOnly
	}
	return types.InvalidTx
}

// ApplyTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
func ApplyTransaction(config *params.ChainConfig, bc ChainContext, author *common.Address, gp *GasPool, statedb *state.DB, header *block.Header, tx *types.Transaction, usedGas *uint64, cfg vm.Config) (*types.Receipt, *types.CXReceipt, uint64, error) {
	txType := getTransactionType(config, header, tx)
	if txType == types.InvalidTx {
		return nil, nil, 0, errors.New("Invalid Transaction Type")
	}

	if txType == types.SubtractionOnly && !config.AcceptsCrossTx(header.Epoch()) {
		return nil, nil, 0, errors.Errorf(
			"cannot handle cross-shard transaction until after epoch %v (now %v)",
			config.CrossTxEpoch, header.Epoch(),
		)
	}

	msg, err := tx.AsMessage(types.MakeSigner(config, header.Epoch()))
	// skip signer err for additiononly tx
	if err != nil {
		return nil, nil, 0, err
	}

	// Create a new context to be used in the EVM environment
	context := NewEVMContext(msg, header, bc, author)
	context.TxType = txType
	// Create a new environment which holds all relevant information
	// about the transaction and calling mechanisms.
	vmenv := vm.NewEVM(context, statedb, config, cfg)
	// Apply the transaction to the current state (included in the env)
	tx.SetBlockNum(header.Number())
	_, gas, failed, err := ApplyMessage(vmenv, msg, gp, bc)
	if err != nil {
		return nil, nil, 0, err
	}
	// Update the state with pending changes
	var root []byte
	if config.IsS3(header.Epoch()) {
		statedb.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(config.IsS3(header.Epoch())).Bytes()
	}
	*usedGas += gas

	// Create a new receipt for the transaction, storing the intermediate root and gas used by the tx
	// based on the eip phase, we're passing whether the root touch-delete accounts.
	receipt := types.NewReceipt(root, failed, *usedGas)
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = gas
	// if the transaction created a contract, store the creation address in the receipt.
	if tx.Type() == types.Contract {
		receipt.ContractAddress = crypto.CreateAddress(vmenv.Context.Origin, tx.Nonce())
	}

	// Set the receipt logs and create a bloom for filtering
	if config.IsReceiptLog(header.Epoch()) {
		receipt.Logs = statedb.GetLogs(tx.Hash())
	}
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})

	var cxReceipt *types.CXReceipt
	// Do not create cxReceipt if EVM call failed
	// not using tx.Type(), as it can be SubstrationOnly, but we should treat as SameShardTx before cross-shard epoch
	if txType == types.SubtractionOnly && !failed {
		if shardID, err := tx.ShardID(); err == nil {
			if toShardID, err := tx.ToShardID(); err == nil {
				cxReceipt = &types.CXReceipt{tx.Hash(), msg.From(), msg.To(), shardID, toShardID, msg.Value()}
			}
		}
	} else {
		cxReceipt = nil
	}

	return receipt, cxReceipt, gas, err
}

// ApplyIncomingReceipt will add amount into ToAddress in the receipt
func ApplyIncomingReceipt(
	config *params.ChainConfig, db *state.DB,
	header *block.Header, cxp *types.CXReceiptsProof,
) error {
	if cxp == nil {
		return nil
	}

	for _, cx := range cxp.Receipts {
		if cx == nil || cx.To == nil { // should not happend
			return errors.Errorf(
				"ApplyIncomingReceipts: Invalid incomingReceipt! %v", cx,
			)
		}
		utils.Logger().Info().Interface("receipt", cx).
			Msgf("ApplyIncomingReceipts: ADDING BALANCE %d", cx.Amount)

		if !db.Exist(*cx.To) {
			db.CreateAccount(*cx.To)
		}
		db.AddBalance(*cx.To, cx.Amount)
		db.IntermediateRoot(config.IsS3(header.Epoch()))
	}
	return nil
}
