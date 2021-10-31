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

package vm

import (
	"fmt"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// Config are the configuration options for the Interpreter
// Config 是 Interpreter 的配置选项
type Config struct {
	// Debug enabled debugging Interpreter options
	// Debug 启用调试 Interpreter 选项
	Debug bool
	// EnableJit enabled the JIT VM
	// EnableJit 启用 JIT VM
	EnableJit bool
	// ForceJit forces the JIT VM
	ForceJit bool
	// Tracer is the op code logger
	Tracer Tracer
	// NoRecursion disabled Interpreter call, callcode,
	// delegate call and create.
	NoRecursion bool
	// Disable gas metering
	DisableGasMetering bool
	// Enable recording of SHA3/keccak preimages
	EnablePreimageRecording bool
	// JumpTable contains the EVM instruction table. This
	// may be left uninitialised and will be set to the default
	// table.
	JumpTable [256]operation
}

// Interpreter is used to run Ethereum based contracts and will utilise the
// passed evmironment to query external sources for state information.
// The Interpreter will run the byte code VM or JIT VM based on the passed
// configuration.
// Interpreter 用于运行基于以太坊的合约，并将利用传递的 evmironment 查询外部源的状态信息。
// Interpreter 将根据传递的配置运行字节码 VM 或 JIT VM。
type Interpreter struct {
	evm      *EVM
	cfg      Config
	// 标识了很多操作的 Gas 价格
	gasTable params.GasTable
	intPool  *intPool

	readOnly   bool   // Whether to throw on stateful modifications
	// 最后一个函数的返回值
	returnData []byte // Last CALL's return data for subsequent reuse
}

// NewInterpreter returns a new instance of the Interpreter.
func NewInterpreter(evm *EVM, cfg Config) *Interpreter {
	// We use the STOP instruction whether to see
	// the jump table was initialised. If it was not
	// we'll set the default jump table.
	// 用一个 STOP 指令测试 JumpTable 是否已经被初始化了, 如果没有被初始化,那么设置为默认值
	if !cfg.JumpTable[STOP].valid {
		switch {
		case evm.ChainConfig().IsByzantium(evm.BlockNumber):
			cfg.JumpTable = byzantiumInstructionSet
		case evm.ChainConfig().IsHomestead(evm.BlockNumber):
			cfg.JumpTable = homesteadInstructionSet
		default:
			cfg.JumpTable = frontierInstructionSet
		}
	}

	return &Interpreter{
		evm:      evm,
		cfg:      cfg,
		gasTable: evm.ChainConfig().GasTable(evm.BlockNumber),
		intPool:  newIntPool(),
	}
}

func (in *Interpreter) enforceRestrictions(op OpCode, operation operation, stack *Stack) error {
	if in.evm.chainRules.IsByzantium {
		if in.readOnly {
			// If the interpreter is operating in readonly mode, make sure no
			// state-modifying operation is performed. The 3rd stack item
			// for a call operation is the value. Transferring value from one
			// account to the others means the state is modified and should also
			// return with an error.
			// 如果解释器在只读模式下运行，请确保不执行状态修改操作。
			// 调用操作的第三个堆栈项是值。 将值从一个帐户转移到其他帐户意味着状态被修改，
			// 并且还应返回错误。
			if operation.writes || (op == CALL && stack.Back(2).BitLen() > 0) {
				return errWriteProtection
			}
		}
	}
	return nil
}

// Run loops and evaluates the contract's code with the given input data and returns
// the return byte-slice and an error if one occurred.
// 用给定的输入参数循环执行合约的代码，并返回返回的字节片段，如果发生错误则返回错误。
// It's important to note that any errors returned by the interpreter should be
// considered a revert-and-consume-all-gas operation. No error specific checks
// should be handled to reduce complexity and errors further down the in.
// 重要的是要注意，解释器返回的任何错误都会消耗全部 gas。 为了减少复杂性,没有特别的错误处理流程。
func (in *Interpreter) Run(snapshot int, contract *Contract, input []byte) (ret []byte, err error) {
	// Increment the call depth which is restricted to 1024
	in.evm.depth++
	defer func() { in.evm.depth-- }()

	// Reset the previous call's return data. It's unimportant to preserve the old buffer
	// as every returning call will return new data anyway.
	// 重置前一次调用的返回数据。 保留旧缓冲区并不重要，因为每次返回调用都会返回新数据。
	in.returnData = nil

	// Don't bother with the execution if there's no code.
	if len(contract.Code) == 0 {
		return nil, nil
	}

	codehash := contract.CodeHash // codehash is used when doing jump dest caching
	if codehash == (common.Hash{}) {
		codehash = crypto.Keccak256Hash(contract.Code)
	}

	var (
		op    OpCode        // current opcode
		mem   = NewMemory() // bound memory
		stack = newstack()  // local stack
		// For optimisation reason we're using uint64 as the program counter.
		// It's theoretically possible to go above 2^64. The YP defines the PC
		// to be uint256. Practically much less so feasible.
		// 出于优化原因，我们使用 uint64 作为程序计数器。
		// 理论上可以超过 2^64。 YP 定义了 PC
		// 为 uint256。 实际上不太可行。
		pc   = uint64(0) // program counter
		cost uint64
		// copies used by tracer
		stackCopy = newstack() // stackCopy needed for Tracer since stack is mutated by 63/64 gas rule
		pcCopy    uint64       // needed for the deferred Tracer
		gasCopy   uint64       // for Tracer to log gas remaining before execution
		logged    bool         // deferred Tracer should ignore already logged steps
	)
	contract.Input = input

	defer func() {
		if err != nil && !logged && in.cfg.Debug {
			in.cfg.Tracer.CaptureState(in.evm, pcCopy, op, gasCopy, cost, mem, stackCopy, contract, in.evm.depth, err)
		}
	}()

	// The Interpreter main run loop (contextual). This loop runs until either an
	// explicit STOP, RETURN or SELFDESTRUCT is executed, an error occurred during
	// the execution of one of the operations or until the done flag is set by the
	// parent context.
	// 解释器的主要循环， 直到遇到 STOP，RETURN，SELFDESTRUCT 指令被执行，
	// 或者是遇到任意错误，或者说 done 标志被父 context 设置。
	for atomic.LoadInt32(&in.evm.abort) == 0 {
		// Get the memory location of pc
		op = contract.GetOp(pc)

		if in.cfg.Debug {
			logged = false
			pcCopy = pc
			gasCopy = contract.Gas
			stackCopy = newstack()
			for _, val := range stack.data {
				stackCopy.push(val)
			}
		}

		// Get the operation from the jump table matching the opcode and validate the
		// stack and make sure there enough stack items available to perform the operation
		// 通过 JumpTable 拿到对应的 operation
		operation := in.cfg.JumpTable[op]
		// 检查指令是否非法
		if !operation.valid {
			return nil, fmt.Errorf("invalid opcode 0x%x", int(op))
		}
		// 检查是否有足够的堆栈空间。 包括入栈和出栈
		if err := operation.validateStack(stack); err != nil {
			return nil, err
		}
		// If the operation is valid, enforce and write restrictions
		// 这里检查了只读模式下面不能执行 writes 指令
		// staticCall 的情况下会设置为 readonly 模式
		if err := in.enforceRestrictions(op, operation, stack); err != nil {
			return nil, err
		}

		var memorySize uint64
		// calculate the new memory size and expand the memory to fit
		// the operation
		// 计算内存使用量，需要收费
		if operation.memorySize != nil {
			memSize, overflow := bigUint64(operation.memorySize(stack))
			if overflow {
				return nil, errGasUintOverflow
			}
			// memory is expanded in words of 32 bytes. Gas
			// is also calculated in words.
			if memorySize, overflow = math.SafeMul(toWordSize(memSize), 32); overflow {
				return nil, errGasUintOverflow
			}
		}
		// 这个参数在本地模拟执行的时候比较有用，可以不消耗或者检查 GAS 执行交易并得到返回结果
		if !in.cfg.DisableGasMetering {
			// consume the gas and return an error if not enough gas is available.
			// cost is explicitly set so that the capture state defer method cas get the proper cost
			// 计算 gas 的 Cost 并使用，如果不够，就返回 OutOfGas 错误。
			cost, err = operation.gasCost(in.gasTable, in.evm, contract, stack, mem, memorySize)
			if err != nil || !contract.UseGas(cost) {
				return nil, ErrOutOfGas
			}
		}
		// 扩大内存范围
		if memorySize > 0 {
			mem.Resize(memorySize)
		}

		if in.cfg.Debug {
			in.cfg.Tracer.CaptureState(in.evm, pc, op, gasCopy, cost, mem, stackCopy, contract, in.evm.depth, err)
			logged = true
		}

		// execute the operation
		res, err := operation.execute(&pc, in.evm, contract, mem, stack)
		// verifyPool is a build flag. Pool verification makes sure the integrity
		// of the integer pool by comparing values to a default value.
		if verifyPool {
			verifyIntegerPool(in.intPool)
		}
		// if the operation clears the return data (e.g. it has returning data)
		// set the last return to the result of the operation.
		// 如果有返回值，那么就设置返回值。 注意只有最后一个返回有效果。
		if operation.returns {
			in.returnData = res
		}

		switch {
		case err != nil:
			return nil, err
		case operation.reverts:
			return res, errExecutionReverted
		case operation.halts:
			return res, nil
		case !operation.jumps:
			pc++
		}
	}
	return nil, nil
}
