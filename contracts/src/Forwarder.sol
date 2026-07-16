// SPDX-License-Identifier: MIT
pragma solidity 0.8.36;

/// @notice Minimal ERC-20 interface — just enough surface to sweep a balance. Avoids
/// pulling in a full OpenZeppelin dependency for two functions.
interface IERC20 {
    function balanceOf(address account) external view returns (uint256);
    function transfer(address to, uint256 amount) external returns (bool);
}

/// @title Forwarder
/// @notice A persistent (EIP-6780-safe — never self-destructs) counterfactual deposit
/// contract, modeled on BitGo's ForwarderV4. Its address is computed off-chain via
/// CREATE2 before it is ever deployed (AD-8) — the "counterfactual" property this whole
/// scheme depends on. Deposits simply accumulate here until Story 3.6's sweep flow
/// deploys this contract for real and flushes it to treasury.
///
/// SWEEP AUTHORITY (finalized in the Story 1.5 code-review rework):
/// flush() and flushToken() are intentionally PERMISSIONLESS but can only ever move funds
/// to the immutable TREASURY constant below. Anyone may trigger a sweep, but no caller can
/// redirect funds elsewhere — the destination is fixed in bytecode, so the worst a griefer
/// can do is pay gas to push funds to their intended resting place. This is the security
/// model the solution design commits to ("deposits land in a contract that can only flush
/// to treasury, not in an EOA whose key must be protected forever", SOLUTION-DESIGN §7).
/// It needs no owner, no access-control, and no Factory relay — an earlier owner-gated
/// draft locked funds permanently because the deploying Factory (the only possible owner)
/// exposed no way to call flush.
///
/// The constructor takes NO arguments: any constructor argument would become part of this
/// contract's creation bytecode, changing the init-code hash per deployment and breaking
/// the one-fixed-hash property every customer's CREATE2 address relies on (AD-8). TREASURY
/// is a compile-time `constant` for the same reason — it is inlined into the bytecode, so
/// it is identical for every Forwarder on every chain.
contract Forwarder {
    /// @notice The sole destination every sweep flushes to.
    ///
    /// PLACEHOLDER — this is NOT the real treasury. The platform's treasury/hot-wallet key
    /// is provisioned at the Story 6.2 key ceremony (its mechanism is "pending, coupled to
    /// the deployment envelope" per the architecture's key inventory). This constant MUST
    /// be replaced with the real hot-wallet address at that ceremony. Because TREASURY is
    /// baked into the bytecode, changing it changes the Forwarder init-code hash and
    /// therefore RE-DERIVES EVERY DEPOSIT ADDRESS. That is acceptable only pre-production,
    /// before any address has been issued to a real customer — it must be finalized before
    /// the first production deposit address is handed out.
    address public constant TREASURY = 0x000000000000000000000000000000000000dEaD;

    event Flushed(address indexed token, address indexed to, uint256 amount);

    /// @notice Accepts native ETH deposits. No logic runs on receipt — attribution and
    /// crediting happen off-chain, in the platform's watcher (Epic 2), by observing this
    /// address; this contract never re-derives or reasons about who sent what.
    receive() external payable {}

    /// @notice Sweeps this contract's entire native ETH balance to TREASURY. Permissionless
    /// (see contract-level note); the destination is fixed, so it cannot be misdirected. No
    /// selfdestruct: this contract is meant to be reused indefinitely (EIP-6780).
    function flush() external {
        uint256 amount = address(this).balance;
        if (amount == 0) {
            return;
        }
        (bool ok, ) = payable(TREASURY).call{value: amount}("");
        require(ok, "Forwarder: ETH transfer failed");
        emit Flushed(address(0), TREASURY, amount);
    }

    /// @notice Sweeps this contract's entire balance of `token` to TREASURY. Permissionless;
    /// the destination is fixed. Uses a low-level call that tolerates non-standard ERC-20s
    /// (e.g. USDT) which return no data from transfer(), instead of a typed bool return that
    /// would revert the ABI decoder and strand those balances forever.
    function flushToken(IERC20 token) external {
        uint256 amount = token.balanceOf(address(this));
        if (amount == 0) {
            return;
        }
        _safeTransfer(token, TREASURY, amount);
        emit Flushed(address(token), TREASURY, amount);
    }

    /// @dev SafeERC20-style transfer: succeeds when the call succeeds AND the token either
    /// returned nothing (non-compliant tokens like USDT) or returned true. Reverts on an
    /// explicit false or a failed call.
    function _safeTransfer(IERC20 token, address to, uint256 amount) private {
        (bool ok, bytes memory data) = address(token).call(
            abi.encodeWithSelector(token.transfer.selector, to, amount)
        );
        require(
            ok && (data.length == 0 || abi.decode(data, (bool))),
            "Forwarder: token transfer failed"
        );
    }
}
