// SPDX-License-Identifier: MIT
pragma solidity 0.8.36;

import {Test, console2} from "forge-std/Test.sol";
import {Factory} from "../src/Factory.sol";
import {Forwarder} from "../src/Forwarder.sol";

/// @notice Cross-language CREATE2 pinning (AC5). The expected addresses asserted here are
/// mirrored, byte-for-byte, in internal/adapter/evm/address_test.go's fixture table — see
/// that file's header comment for the cross-reference. If Factory.sol or Forwarder.sol
/// ever change in a way that changes their creation bytecode, every expected value below
/// (and its Go-side twin) must be regenerated together: temporarily add
/// `console2.log(...)` calls (or `-vvvv` with `console2.logBytes32`) to print the fresh
/// init-code hashes and addresses, then copy them into both files.
///
/// TEST_PLATFORM_FACTORY is an arbitrary fixed address standing in for wherever the
/// platform's real Factory ends up deployed via CREATE2 from the canonical deployer
/// (computed for real in Go, internal/adapter/evm/address.go — no real deployment happens
/// in this story). This test only needs A fixed, known address to prove the CREATE2
/// formula itself is byte-identical between Solidity and Go — it does not need to be the
/// actual production address.
contract CreateAddressVectorsTest is Test {
    address constant TEST_PLATFORM_FACTORY = address(0x0000000000000000000000000000000000133700);

    // The architecture spine's prose cites this address as
    // "0x4e59b44847B379578588920cA78FbF26c0B4956C" — solc rejects that exact casing as an
    // invalid EIP-55 checksum (a compile error, not a warning). The checksum below is
    // solc's own correction for the same 20 bytes; this typo must be fixed everywhere else
    // this address is cited (Go constants, story text) — see the story's Dev Agent Record.
    address constant CANONICAL_DEPLOYER = address(0x4e59b44847b379578588920cA78FbF26c0B4956C);
    bytes32 constant FACTORY_SALT = bytes32(0);

    /// @notice The REAL platform Factory address — CREATE2(CANONICAL_DEPLOYER, FACTORY_SALT,
    /// keccak256(Factory creationCode)), i.e. exactly what
    /// internal/adapter/evm/address.go computes as `platformFactoryAddress` (and what
    /// testPlatformFactoryAddressMatchesExpected pins). Used by the production-tuple test
    /// below so the exact derivation `DeriveAddress` performs in Go is asserted directly,
    /// not just compositionally.
    address constant PLATFORM_FACTORY = 0xCc0939512Fdb0811bD89aB1E13D6bB131AC3e7A7;

    Factory internal factory;
    Factory internal productionFactory;

    function setUp() public {
        Factory deployed = new Factory();
        // Place the real Factory's runtime code at our fixed test address so
        // computeAddress()'s internal `address(this)` resolves to TEST_PLATFORM_FACTORY —
        // exercising the actual contract logic, not a reimplementation of it, against a
        // deterministic address.
        vm.etch(TEST_PLATFORM_FACTORY, address(deployed).code);
        factory = Factory(TEST_PLATFORM_FACTORY);
        // Same trick at the REAL platform factory address, for the production-tuple test.
        vm.etch(PLATFORM_FACTORY, address(deployed).code);
        productionFactory = Factory(PLATFORM_FACTORY);
    }

    /// @notice Salt scheme per AD-8: the customer UUID's 16 bytes, left-padded with zeros
    /// to bytes32. customerUUIDLow128 packs the UUID's 16 bytes into a uint128.
    function _salt(uint128 customerUUIDLow128) internal pure returns (bytes32) {
        return bytes32(uint256(customerUUIDLow128));
    }

    function _computeCreate2(address deployer, bytes32 salt, bytes32 initCodeHash)
        internal
        pure
        returns (address)
    {
        return address(
            uint160(uint256(keccak256(abi.encodePacked(bytes1(0xff), deployer, salt, initCodeHash))))
        );
    }

    /// @notice Pins the platform Factory's own CREATE2 address — computed against the
    /// real canonical deployer and a fixed factory-salt, exactly as internal/adapter/evm
    /// computes it in Go. Mirrored in address_test.go as `wantPlatformFactoryAddress`.
    function testPlatformFactoryAddressMatchesExpected() public pure {
        address computed = _computeCreate2(CANONICAL_DEPLOYER, FACTORY_SALT, keccak256(type(Factory).creationCode));
        assertEq(computed, 0xCc0939512Fdb0811bD89aB1E13D6bB131AC3e7A7, "platform factory address mismatch");
    }

    /// @notice Pins a handful of customer forwarder addresses against a fixed test
    /// factory. Mirrored in address_test.go's fixture table.
    function testForwarderAddressesMatchExpected() public view {
        uint128[3] memory customerUUIDs = [
            uint128(0x00000000000000000000000000000001),
            uint128(0x11111111111111111111111111111111),
            uint128(0x018f66c96433_72ea_a81e_e41bba23bc27)
        ];
        address[3] memory expected = [
            0x3fBd234cBCe33F5E0E6DE317a18f83391043243f,
            0x87C0A5ed755da1D0Ab7Bcdb4C5b659abb6f32dA1,
            0xda1b72527005D50a97320CDB8D276eDEDAaF4da7
        ];

        for (uint256 i = 0; i < customerUUIDs.length; i++) {
            assertEq(
                factory.computeAddress(_salt(customerUUIDs[i])),
                expected[i],
                "forwarder address mismatch"
            );
        }
    }

    /// @notice Pins the EXACT production derivation tuple —
    /// CREATE2(PLATFORM_FACTORY, customerSalt, forwarderInitCodeHash) — the precise call
    /// internal/adapter/evm's DeriveAddress makes for every real customer. The other
    /// vectors pin each ingredient separately (formula, init-code hash, factory address);
    /// this one pins their composition. Mirrored in address_test.go's
    /// TestDeriveAddress_MatchesForgeProductionVectors.
    function testProductionForwarderAddressesMatchExpected() public view {
        uint128[3] memory customerUUIDs = [
            uint128(0x00000000000000000000000000000001),
            uint128(0x11111111111111111111111111111111),
            uint128(0x018f66c96433_72ea_a81e_e41bba23bc27)
        ];
        address[3] memory expected = [
            0x23392da92cB99a92a67866933F56A1FF1a0c2B8F,
            0x69F1091ac791Be6cbF11fD57Ed88A69e0cD16aC6,
            0xdcB2d52518a576007e1Ccf439e2b8020Ab13F6a3
        ];

        for (uint256 i = 0; i < customerUUIDs.length; i++) {
            assertEq(
                productionFactory.computeAddress(_salt(customerUUIDs[i])),
                expected[i],
                "production forwarder address mismatch"
            );
        }
    }

    /// @notice NOT an assertion — this is the committed regeneration procedure for every
    /// pinned vector in this file and in internal/adapter/evm/address.go /
    /// address_test.go (referenced from address.go's header comment). Whenever
    /// Forwarder.sol or Factory.sol bytecode changes (which re-derives every customer
    /// address — AD-8), run:
    ///
    ///   forge test --match-test testPrintVectors -vv --json
    ///
    /// and copy the printed values programmatically (never by eye from wrapped terminal
    /// output — a dropped hex digit has already happened once, see the story's Dev Agent
    /// Record) into: forwarderInitCodeHash / factoryInitCodeHash / the platform factory
    /// address in address.go, and the expected addresses in this file's tests and
    /// address_test.go's fixture tables.
    function testPrintVectors() public view {
        console2.log("forwarderInitCodeHash:");
        console2.logBytes32(keccak256(type(Forwarder).creationCode));
        console2.log("factoryInitCodeHash:");
        console2.logBytes32(keccak256(type(Factory).creationCode));
        console2.log(
            "platformFactoryAddress:",
            _computeCreate2(CANONICAL_DEPLOYER, FACTORY_SALT, keccak256(type(Factory).creationCode))
        );

        uint128[3] memory customerUUIDs = [
            uint128(0x00000000000000000000000000000001),
            uint128(0x11111111111111111111111111111111),
            uint128(0x018f66c96433_72ea_a81e_e41bba23bc27)
        ];
        for (uint256 i = 0; i < customerUUIDs.length; i++) {
            console2.log("test-factory forwarder:", factory.computeAddress(_salt(customerUUIDs[i])));
            console2.log("production forwarder:", productionFactory.computeAddress(_salt(customerUUIDs[i])));
        }
    }
}
