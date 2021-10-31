// Copyright 2014 The go-ethereum Authors
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
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

var (
	Big0                         = big.NewInt(0)
	errInsufficientBalanceForGas = errors.New("insufficient balance to pay for gas")
)

/*
The State Transitioning Model
状态转换模型

A state transition is a change made when a transaction is applied to the current world state
状态转换 是指用当前的 world state 来执行交易，并改变当前的 world state
The state transitioning model does all all the necessary work to work out a valid new state root.
状态转换做了所有所需的工作来产生一个新的有效的 state root

1) Nonce handling	Nonce 处理
2) Pre pay gas		预先支付 Gas
3) Create a new state object if the recipient is \0*32	如果接收人是空，那么创建一个新的 state object
4) Value transfer	转账
== If contract creation ==
  4a) Attempt to run transaction data	尝试运行输入的数据
  4b) If valid, use result as code for the new state object	如果有效，那么用运行的结果作为新的 state object 的 code
== end ==
5) Run Script section	运行脚本部分
6) Derive new state root	导出新的 state root
*/
type StateTransition struct {
	//  用来追踪区块内部的 Gas 的使用情况
	gp         *GasPool
	// Message Call
	msg        Message
	gas        uint64
	// gas 的价格
	gasPrice   *big.Int
	// 最开始的 gas
	initialGas *big.Int
	// 转账的值
	value      *big.Int
	// 输入数据
	data       []byte
	// StateDB
	state      vm.StateDB
	// 虚拟机
	evm        *vm.EVM
}

// Message represents a message sent to a contract.
type Message interface {
	From() common.Address
	//FromFrontier() (common.Address, error)
	To() *common.Address
	// Message 的 GasPrice
	GasPrice() *big.Int
	// message 的 GasLimit
	Gas() *big.Int
	Value() *big.Int

	Nonce() uint64
	CheckNonce() bool
	Data() []byte
}

// IntrinsicGas computes the 'intrinsic gas' for a message
// with the given data.
// IntrinsicGas 计算具有给定数据的消息的“intrinsic gas”。
// TODO convert to uint64
func IntrinsicGas(data []byte, contractCreation, homestead bool) *big.Int {
	igas := new(big.Int)
	if contractCreation && homestead {
		// Gtxcreate + Gtransaction = TxGasContractCreation
		igas.SetUint64(params.TxGasContractCreation)
	} else {
		igas.SetUint64(params.TxGas)
	}
	if len(data) > 0 {
		var nz int64
		for _, byt := range data {
			if byt != 0 {
				nz++
			}
		}
		m := big.NewInt(nz)
		m.Mul(m, new(big.Int).SetUint64(params.TxDataNonZeroGas))
		igas.Add(igas, m)
		m.SetInt64(int64(len(data)) - nz)
		m.Mul(m, new(big.Int).SetUint64(params.TxDataZeroGas))
		igas.Add(igas, m)
	}
	return igas
}

// NewStateTransition initialises and returns a new state transition object.
// NewStateTransition 初始化并返回一个新的状态转换对象。
func NewStateTransition(evm *vm.EVM, msg Message, gp *GasPool) *StateTransition {
	return &StateTransition{
		gp:         gp,
		evm:        evm,
		msg:        msg,
		gasPrice:   msg.GasPrice(),
		initialGas: new(big.Int),
		value:      msg.Value(),
		data:       msg.Data(),
		state:      evm.StateDB,
	}
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any EVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
// ApplyMessage 通过应用给定的 message 和状态来生成新的状态
//
// ApplyMessage 返回任何 EVM 执行返回的字节（如果发生）、
// 使用的gas（包括 gas 退款）以及失败时的错误。 错误始终表示核心错误，
// 这意味着该消息对于该特定状态将始终失败，并且永远不会在块内被接受。
func ApplyMessage(evm *vm.EVM, msg Message, gp *GasPool) ([]byte, *big.Int, bool, error) {
	st := NewStateTransition(evm, msg, gp)

	ret, _, gasUsed, failed, err := st.TransitionDb()
	return ret, gasUsed, failed, err
}

func (st *StateTransition) from() vm.AccountRef {
	f := st.msg.From()
	if !st.state.Exist(f) {
		st.state.CreateAccount(f)
	}
	return vm.AccountRef(f)
}

func (st *StateTransition) to() vm.AccountRef {
	if st.msg == nil {
		return vm.AccountRef{}
	}
	to := st.msg.To()
	if to == nil {
		return vm.AccountRef{} // contract creation
	}

	reference := vm.AccountRef(*to)
	if !st.state.Exist(*to) {
		st.state.CreateAccount(*to)
	}
	return reference
}

func (st *StateTransition) useGas(amount uint64) error {
	if st.gas < amount {
		return vm.ErrOutOfGas
	}
	st.gas -= amount

	return nil
}

//  实现 Gas 的预扣费
func (st *StateTransition) buyGas() error {
	mgas := st.msg.Gas()
	if mgas.BitLen() > 64 {
		return vm.ErrOutOfGas
	}

	mgval := new(big.Int).Mul(mgas, st.gasPrice)

	var (
		state  = st.state
		sender = st.from()
	)
	if state.GetBalance(sender.Address()).Cmp(mgval) < 0 {
		return errInsufficientBalanceForGas
	}
	// 从区块的 gaspool 里面减去， 因为区块是由 GasLimit 限制整个区块的 Gas 使用的。
	if err := st.gp.SubGas(mgas); err != nil {
		return err
	}
	st.gas += mgas.Uint64()

	st.initialGas.Set(mgas)
	// 从账号里面减去 GasLimit * GasPrice
	state.SubBalance(sender.Address(), mgval)
	return nil
}

// 执行前的检查
func (st *StateTransition) preCheck() error {
	msg := st.msg
	sender := st.from()

	// Make sure this transaction's nonce is correct
	if msg.CheckNonce() {
		nonce := st.state.GetNonce(sender.Address())
		// 当前本地的 nonce 需要和 msg 的 Nonce 一样 不然就是状态不同步了。
		if nonce < msg.Nonce() {
			return ErrNonceTooHigh
		} else if nonce > msg.Nonce() {
			return ErrNonceTooLow
		}
	}
	return st.buyGas()
}

// TransitionDb will transition the state by applying the current message and returning the result
// including the required gas for the operation as well as the used gas. It returns an error if it
// failed. An error indicates a consensus issue.
// TransitionDb 将通过应用当前消息并返回结果来转换状态，
// 包括操作所需的 gas 以及使用的 gas。 如果失败，它会返回一个错误。
// 错误表示共识问题。
func (st *StateTransition) TransitionDb() (ret []byte, requiredGas, usedGas *big.Int, failed bool, err error) {
	if err = st.preCheck(); err != nil {
		return
	}
	msg := st.msg
	sender := st.from() // err checked in preCheck

	homestead := st.evm.ChainConfig().IsHomestead(st.evm.BlockNumber)
	// 如果 msg.To 是 nil 那么认为是一个合约创建
	contractCreation := msg.To() == nil

	// Pay intrinsic gas
	// TODO convert to uint64
	// 计算最开始的 Gas  g0
	intrinsicGas := IntrinsicGas(st.data, contractCreation, homestead)
	if intrinsicGas.BitLen() > 64 {
		return nil, nil, nil, false, vm.ErrOutOfGas
	}
	if err = st.useGas(intrinsicGas.Uint64()); err != nil {
		return nil, nil, nil, false, err
	}

	var (
		evm = st.evm
		// vm errors do not effect consensus and are therefor
		// not assigned to err, except for insufficient balance
		// error.
		vmerr error
	)
	// 如果是合约创建， 那么调用 evm 的 Create 方法
	if contractCreation {
		ret, _, st.gas, vmerr = evm.Create(sender, st.data, st.gas, st.value)
	} else {
		// Increment the nonce for the next transaction
		// // 如果是方法调用。那么首先设置 sender 的 nonce。
		st.state.SetNonce(sender.Address(), st.state.GetNonce(sender.Address())+1)
		ret, st.gas, vmerr = evm.Call(sender, st.to().Address(), st.data, st.gas, st.value)
	}
	if vmerr != nil {
		log.Debug("VM returned with error", "err", vmerr)
		// The only possible consensus-error would be if there wasn't
		// sufficient balance to make the transfer happen. The first
		// balance transfer may never fail.
		if vmerr == vm.ErrInsufficientBalance {
			return nil, nil, nil, false, vmerr
		}
	}
	// 计算被使用的 Gas 数量
	requiredGas = new(big.Int).Set(st.gasUsed())
	// 计算 Gas 的退费 会增加到 st.gas 上面。 所以矿工拿到的是退税后的
	st.refundGas()
	// 给矿工增加收入。
	st.state.AddBalance(st.evm.Coinbase, new(big.Int).Mul(st.gasUsed(), st.gasPrice))
	// requiredGas 和 gasUsed 的区别一个是没有退税的， 一个是退税了的。
	// 看上面的调用 ApplyMessage 直接丢弃了 requiredGas, 说明返回的是退税了的。
	return ret, requiredGas, st.gasUsed(), vmerr != nil, err
}

func (st *StateTransition) refundGas() {
	// Return eth for remaining gas to the sender account,
	// exchanged at the original rate.
	// 将剩余 gas 的 eth 返还至发送方账户，按原汇率兑换。
	sender := st.from() // err already checked
	remaining := new(big.Int).Mul(new(big.Int).SetUint64(st.gas), st.gasPrice)
	st.state.AddBalance(sender.Address(), remaining)

	// Apply refund counter, capped to half of the used gas.
	// 应用退款计数器，上限为已用 gas 的一半。
	uhalf := remaining.Div(st.gasUsed(), common.Big2)
	refund := math.BigMin(uhalf, st.state.GetRefund())
	st.gas += refund.Uint64()

	st.state.AddBalance(sender.Address(), refund.Mul(refund, st.gasPrice))

	// Also return remaining gas to the block gas counter so it is
	// available for the next transaction.
	st.gp.AddGas(new(big.Int).SetUint64(st.gas))
}

// 计算已使用的 gas
func (st *StateTransition) gasUsed() *big.Int {
	return new(big.Int).Sub(st.initialGas, new(big.Int).SetUint64(st.gas))
}
