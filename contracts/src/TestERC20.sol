// SPDX-License-Identifier: MIT
pragma solidity 0.8.36;

/// @notice Minimal standard-shaped ERC-20 fixture used ONLY by
/// internal/adapter/evm/scanner_test.go's real-anvil test, to exercise ScanDeposits'
/// eth_getLogs Transfer-log path (USDC detection) against a real, standard-shaped
/// Transfer(address indexed from, address indexed to, uint256 value) event on a real
/// chain. Never deployed anywhere real — not part of the platform's own on-chain
/// contracts (Factory.sol / Forwarder.sol) and not referenced by AD-8's CREATE2 scheme.
/// Its creation bytecode is embedded as a Go constant (testERC20CreationBytecode in
/// scanner_test.go) via the same "compile once, pin the output" approach address.go
/// uses for Forwarder/Factory's init-code hashes — see this file's header for the
/// regeneration command if this contract's source ever changes.
///
/// Regeneration procedure: `forge build` from contracts/, then copy the hex string at
/// contracts/out/TestERC20.sol/TestERC20.json's ".bytecode.object" field into
/// scanner_test.go's testERC20CreationBytecode constant.
contract TestERC20 {
    event Transfer(address indexed from, address indexed to, uint256 value);

    mapping(address => uint256) public balanceOf;

    constructor(uint256 initialSupply) {
        balanceOf[msg.sender] = initialSupply;
        emit Transfer(address(0), msg.sender, initialSupply);
    }

    function transfer(address to, uint256 amount) external returns (bool) {
        require(balanceOf[msg.sender] >= amount, "TestERC20: insufficient balance");
        balanceOf[msg.sender] -= amount;
        balanceOf[to] += amount;
        emit Transfer(msg.sender, to, amount);
        return true;
    }
}
