package l2

import (
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
)

var (
	DepositEventABI     = "TransactionDeposited(address,address,uint256,uint256,uint256,bool,bytes)"
	DepositEventABIHash = crypto.Keccak256Hash([]byte(DepositEventABI))
	DepositContractAddr = common.HexToAddress("0xdeaddeaddeaddeaddeaddeaddeaddeaddead0001")
	L1InfoFuncSignature = "setL1BlockValues(uint256 _number, uint256 _timestamp, uint256 _basefee, bytes32 _hash)"
	L1InfoFuncBytes4    = crypto.Keccak256([]byte(L1InfoFuncSignature))[:4]
	L1InfoPredeployAddr = common.HexToAddress("0x4242424242424242424242424242424242424242")
)

// UnmarshalLogEvent decodes an EVM log entry emitted by the deposit contract into typed deposit data.
//
// parse log data for:
//     event TransactionDeposited(
//    	 address indexed from,
//    	 address indexed to,
//       uint256 mint,
//    	 uint256 value,
//    	 uint256 gasLimit,
//    	 bool isCreation,
//    	 data data
//     );
//
// Deposits additionally get:
//  - blockNum matching the L1 block height
//  - txIndex: matching the deposit index, not L1 transaction index, since there can be multiple deposits per L1 tx
func UnmarshalLogEvent(blockNum uint64, txIndex uint64, ev *types.Log) (*types.DepositTx, error) {
	if len(ev.Topics) != 3 {
		return nil, fmt.Errorf("expected 3 event topics (event identity, indexed from, indexed to)")
	}
	if ev.Topics[0] != DepositEventABIHash {
		return nil, fmt.Errorf("invalid deposit event selector: %s, expected %s", ev.Topics[0], DepositEventABIHash)
	}
	if len(ev.Data) < 6*32 {
		return nil, fmt.Errorf("deposit event data too small (%d bytes): %x", len(ev.Data), ev.Data)
	}

	var dep types.DepositTx

	dep.BlockHeight = blockNum
	dep.TransactionIndex = txIndex

	// indexed 0
	dep.From = common.BytesToAddress(ev.Topics[1][12:])
	// indexed 1
	to := common.BytesToAddress(ev.Topics[2][12:])

	// unindexed data
	offset := uint64(0)
	dep.Value = new(big.Int).SetBytes(ev.Data[offset : offset+32])
	offset += 32

	dep.Mint = new(big.Int).SetBytes(ev.Data[offset : offset+32])
	// 0 mint is represented as nil to skip minting code
	if dep.Mint.Cmp(new(big.Int)) == 0 {
		dep.Mint = nil
	}
	offset += 32

	gas := new(big.Int).SetBytes(ev.Data[offset : offset+32])
	if !gas.IsUint64() {
		return nil, fmt.Errorf("bad gas value: %x", ev.Data[offset:offset+32])
	}
	offset += 32
	dep.Gas = gas.Uint64()
	// isCreation: If the boolean byte is 1 then dep.To will stay nil,
	// and it will create a contract using L2 account nonce to determine the created address.
	if ev.Data[offset+31] == 0 {
		dep.To = &to
	}
	offset += 32
	var dataOffset uint256.Int
	dataOffset.SetBytes(ev.Data[offset : offset+32])
	offset += 32
	if dataOffset.Eq(uint256.NewInt(128)) {
		return nil, fmt.Errorf("incorrect data offset: %v", dataOffset[0])
	}

	var dataLen uint256.Int
	dataLen.SetBytes(ev.Data[offset : offset+32])
	offset += 32

	if !dataLen.IsUint64() {
		return nil, fmt.Errorf("data too large: %s", dataLen.String())
	}
	// The data may be padded to a multiple of 32 bytes
	maxExpectedLen := uint64(len(ev.Data)) - offset
	dataLenU64 := dataLen.Uint64()
	if dataLenU64 > maxExpectedLen {
		return nil, fmt.Errorf("data length too long: %d, expected max %d", dataLenU64, maxExpectedLen)
	}

	// remaining bytes fill the data
	dep.Data = ev.Data[offset : offset+dataLenU64]

	return &dep, nil
}

type L1Info interface {
	NumberU64() uint64
	Time() uint64
	Hash() common.Hash
	BaseFee() *big.Int
}

func DeriveL1InfoDeposit(block L1Info) *types.DepositTx {
	data := make([]byte, 4+8+8+32+32)
	offset := 0
	copy(data[offset:4], L1InfoFuncBytes4)
	offset += 4
	binary.BigEndian.PutUint64(data[offset:offset+8], block.NumberU64())
	offset += 8
	binary.BigEndian.PutUint64(data[offset:offset+8], block.Time())
	offset += 8
	block.BaseFee().FillBytes(data[offset : offset+32])
	offset += 32
	copy(data[offset:offset+32], block.Hash().Bytes())

	return &types.DepositTx{
		BlockHeight:      block.NumberU64(),
		TransactionIndex: 0, // always the first transaction
		From:             DepositContractAddr,
		To:               &L1InfoPredeployAddr,
		Mint:             nil,
		Value:            big.NewInt(0),
		Gas:              99_999_999,
		Data:             data,
	}
}

type ReceiptHash interface {
	ReceiptHash() common.Hash
}

// CheckReceipts sanity checks that the receipts are consistent with the block data.
func CheckReceipts(block ReceiptHash, receipts []*types.Receipt) bool {
	hasher := trie.NewStackTrie(nil)
	computed := types.DeriveSha(types.Receipts(receipts), hasher)
	return block.ReceiptHash() == computed
}

// DeriveL2Transactions transforms a L1 block and corresponding receipts into the transaction inputs for a full L2 block
func DeriveUserDeposits(height uint64, receipts []*types.Receipt) ([]*types.DepositTx, error) {
	var out []*types.DepositTx

	for _, rec := range receipts {
		if rec.Status != types.ReceiptStatusSuccessful {
			continue
		}
		for _, log := range rec.Logs {
			if log.Address == DepositContractAddr {
				// offset transaction index by 1, the first is the l1-info tx
				dep, err := UnmarshalLogEvent(height, uint64(len(out))+1, log)
				if err != nil {
					return nil, fmt.Errorf("malformatted L1 deposit log: %v", err)
				}
				out = append(out, dep)
			}
		}
	}
	return out, nil
}

type BlockInput interface {
	ReceiptHash
	L1Info
	MixDigest() common.Hash
}

func DeriveBlockInputs(block BlockInput, receipts []*types.Receipt) (*PayloadAttributes, error) {
	if !CheckReceipts(block, receipts) {
		return nil, fmt.Errorf("receipts are not consistent with the block's receipts root: %s", block.ReceiptHash())
	}

	l1Tx := types.NewTx(DeriveL1InfoDeposit(block))
	opaqueL1Tx, err := l1Tx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("failed to encode L1 info tx")
	}

	userDeposits, err := DeriveUserDeposits(block.NumberU64(), receipts)
	if err != nil {
		return nil, fmt.Errorf("failed to derive user deposits: %v", err)
	}

	encodedTxs := make([]Data, 0, len(userDeposits)+1)
	encodedTxs = append(encodedTxs, opaqueL1Tx)

	for i, tx := range userDeposits {
		opaqueTx, err := types.NewTx(tx).MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("failed to encode user tx %d", i)
		}
		encodedTxs = append(encodedTxs, opaqueTx)
	}

	return &PayloadAttributes{
		Timestamp:             Uint64Quantity(block.Time()),
		Random:                Bytes32(block.MixDigest()),
		SuggestedFeeRecipient: common.Address{}, // nobody gets tx fees for deposits
		Transactions:          encodedTxs,
	}, nil
}
