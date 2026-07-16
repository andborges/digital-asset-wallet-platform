// SPDX-License-Identifier: MIT
pragma solidity 0.8.36;

import {Forwarder} from "./Forwarder.sol";

/// @title Factory
/// @notice Deploys per-customer Forwarder contracts via CREATE2, keyed by a
/// caller-supplied salt (the customer UUID left-padded to bytes32, per AD-8's salt
/// scheme — computed off-chain in internal/core). This contract's own address must be
/// identical on every supported chain (AD-8): deploying it through the canonical
/// deterministic deployer (0x4e59b44847b379578588920cA78FbF26c0B4956C) with a fixed salt
/// achieves that, since a CREATE2 address depends only on (deployer, salt, init code
/// hash) — never on chain ID.
contract Factory {
    event ForwarderDeployed(bytes32 indexed salt, address forwarder);

    /// @notice Deploys (or re-returns, if already deployed) the Forwarder for `salt`.
    /// Story 3.6 is the only real caller in production — this story only needs the
    /// bytecode to be final, not for this function to ever be invoked yet.
    function deploy(bytes32 salt) external returns (address forwarder) {
        forwarder = computeAddress(salt);
        if (forwarder.code.length == 0) {
            Forwarder deployed = new Forwarder{salt: salt}();
            require(address(deployed) == forwarder, "Factory: address mismatch");
            emit ForwarderDeployed(salt, forwarder);
        }
    }

    /// @notice Computes the CREATE2 address of the Forwarder for `salt`, without
    /// deploying it — the on-chain mirror of `internal/adapter/evm`'s Go computation,
    /// and this project's cross-language pinning test (AC5).
    function computeAddress(bytes32 salt) public view returns (address) {
        bytes32 initCodeHash = keccak256(type(Forwarder).creationCode);
        return address(
            uint160(
                uint256(
                    keccak256(abi.encodePacked(bytes1(0xff), address(this), salt, initCodeHash))
                )
            )
        );
    }
}
