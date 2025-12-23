// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

/**
 * @title JoyueLib
 * @dev Solidity SDK for JOYUE V1.4 §6: "assert wrapper" + "if-else conditionals"
 *      that generate Guards/Deltas while executing business logic on an Agent (cache-based) contract.
 *
 * IMPORTANT:
 * - Agent reads state ONLY from cache (P2P cache in the real system).
 * - Agent outputs an Intent (Guards + Deltas + RawRequest) to be sent to Master for authoritative verify/commit.
 *
 * TODO(P2P Cache):
 * - Replace `IJoyueCache` mock interface integration with node-level P2P cache plumbing.
 * - Each node will maintain cache entries for (contractAddr,key) -> (value, version).
 * - Solidity should only call the cache contract (or a precompile) exposed by the node.
 */
// P2P cache接口（读）
interface IJoyueCache {
    function get(address contractAddr, bytes32 key) external view returns (bytes memory value, uint64 version, bool ok);
}

// 可选写接口（用于 Agent 侧 mock/可写缓存快进）
interface IJoyueCacheWriter {
    function setUint(address contractAddr, bytes32 key, uint256 value, uint64 version) external;
}

library JoyueLib {
    // ============================================================
    //                      Enums / Consts
    // ============================================================

    // Guard ops (V1.4 §5.1)
    uint8 public constant OP_EQ = 0x01;
    uint8 public constant OP_NEQ = 0x02;
    uint8 public constant OP_GT = 0x03;
    uint8 public constant OP_GTE = 0x04;
    uint8 public constant OP_LT = 0x05;
    uint8 public constant OP_LTE = 0x06;
    uint8 public constant OP_EXPR = 0xFF; // RPN expression guard

    // Delta ops (V1.4 §5.1)
    uint8 public constant D_ADD = 0x01;
    uint8 public constant D_SUB = 0x02;
    uint8 public constant D_SET = 0x10;
    uint8 public constant D_BIT_OR = 0x20;
    uint8 public constant D_BIT_CLEAR = 0x21;
    uint8 public constant D_BIT_XOR = 0x22;
    uint8 public constant D_LOG = 0xFF;

    // Guard strategy (V1.4 §6.1.2)
    uint8 public constant STRATEGY_STRICT = 0x00;
    uint8 public constant STRATEGY_RELAXED = 0x01;

    // Expr token kinds (for OP_EXPR payload)
    uint8 internal constant TOK_VAR = 0x01;   // (contractAddr,key,version)
    uint8 internal constant TOK_ARG = 0x02;   // (argIndex)
    uint8 internal constant TOK_CONST = 0x03; // (uint256)
    uint8 internal constant TOK_OP = 0x04;    // (opcode)

    // Expr math ops (minimal set for demo)
    uint8 internal constant EXPR_ADD = 0x01;
    uint8 internal constant EXPR_SUB = 0x02;
    uint8 internal constant EXPR_MUL = 0x03;
    uint8 internal constant EXPR_DIV = 0x04;

    // ============================================================
    //                      Data Structures
    // ============================================================

    struct Guard {
        address contractAddr; // target contract (Master) whose state is guarded
        bytes32 key;          // state key
        uint64 readVersion;   // cache version read by agent
        uint8 strategy;       // STRICT / RELAXED
        uint8 op;             // OP_*
        bytes val;            // abi.encode(uint256) for atomic, or abi-encoded RPN payload for OP_EXPR
    }

    struct Delta {
        address contractAddr; // target contract (Master) whose state will be modified
        bytes32 key;
        uint8 op;             // D_*
        bytes val;            // abi.encode(uint256) / bytes
    }

    /**
     * @dev RawRequest is the "retry base": Master uses (targetAddr, selector, args) to replay logic tree.
     */
    struct RawRequest {
        address targetAddr; // Master contract entry
        bytes4 selector;
        bytes args;         // original function args (abi.encode(...))
    }

    struct Context {
        Guard[] guards;
        Delta[] deltas;
        RawRequest req;
        address cacheAddr;  // cache contract / precompile address exposed by node
    }

    /**
     * @dev JVar wraps a state variable read from cache.
     */
    struct JVar {
        address contractAddr;
        bytes32 key;
        bytes value;
        uint64 version;
        bool loaded;
    }

    /**
     * @dev RPN expression builder. It also evaluates using cached values to decide branch direction,
     * while emitting an OP_EXPR Guard with correct comparator (positive or inverse).
     */
    struct ExprBuilder {
        bytes[] tokens;      // serialized tokens
        uint256[] evalStack; // runtime eval stack (cached values)
    }

    // ============================================================
    //                      Events
    // ============================================================

    event IntentEmitted(
        address indexed targetAddr,
        bytes4 indexed selector,
        bytes args,
        Guard[] guards,
        Delta[] deltas
    );

    // ============================================================
    //                      Context
    // ============================================================

    function newContext(address cacheAddr, address targetAddr, bytes4 selector, bytes memory args)
        internal
        pure
        returns (Context memory ctx)
    {
        ctx.cacheAddr = cacheAddr;
        ctx.req = RawRequest({targetAddr: targetAddr, selector: selector, args: args});
    }

    function emitIntent(Context memory ctx) internal {
        emit IntentEmitted(ctx.req.targetAddr, ctx.req.selector, ctx.req.args, ctx.guards, ctx.deltas);
    }

    // ============================================================
    //                      Cache Load
    // ============================================================

    function loadFromCacheByKey(Context memory ctx, address contractAddr, bytes32 key)
        internal
        view
        returns (JVar memory v)
    {
        v.contractAddr = contractAddr;
        v.key = key;

        // TODO(P2P Cache): in production this must read from node-provided P2P cache.
        require(ctx.cacheAddr != address(0), "JOYUE: cacheAddr not set");
        (bytes memory value, uint64 ver, bool ok) = IJoyueCache(ctx.cacheAddr).get(contractAddr, key);
        if (ok) {
            v.value = value;
            v.version = ver;
        } else {
            v.value = bytes("");
            v.version = 0;
        }
        v.loaded = true;
        return v;
    }

    // ============================================================
    //                      Key Helpers
    // ============================================================

    function keyOf(string memory s) internal pure returns (bytes32) {
        return keccak256(abi.encodePacked(s));
    }

    function keyOf2(string memory prefix, bytes32 x) internal pure returns (bytes32) {
        return keccak256(abi.encodePacked(prefix, x));
    }

    function keyOfAddr(string memory prefix, address a) internal pure returns (bytes32) {
        return keccak256(abi.encodePacked(prefix, a));
    }

    // ============================================================
    //                      Value Helpers
    // ============================================================

    function toUint(bytes memory b) internal pure returns (uint256) {
        if (b.length < 32) return 0;
        return abi.decode(b, (uint256));
    }

    function _pushGuard(Context memory ctx, Guard memory g) private pure {
        Guard[] memory oldG = ctx.guards;
        Guard[] memory newG = new Guard[](oldG.length + 1);
        for (uint256 i = 0; i < oldG.length; i++) {
            newG[i] = oldG[i];
        }
        newG[oldG.length] = g;
        ctx.guards = newG;
    }

    function _addGuard(
        Context memory ctx,
        JVar memory v,
        uint8 op,
        uint256 threshold,
        uint8 strategy
    ) private pure {
        _pushGuard(
            ctx,
            Guard({
                contractAddr: v.contractAddr,
                key: v.key,
                readVersion: v.version,
                strategy: strategy,
                op: op,
                val: abi.encode(threshold)
            })
        );
    }

    function _inverseAtomic(uint8 op) private pure returns (uint8) {
        if (op == OP_GT) return OP_LTE;
        if (op == OP_GTE) return OP_LT;
        if (op == OP_LT) return OP_GTE;
        if (op == OP_LTE) return OP_GT;
        if (op == OP_EQ) return OP_NEQ;
        if (op == OP_NEQ) return OP_EQ;
        return op;
    }

    // ============================================================
    //                      Conditionals (if-else)
    // ============================================================

    function checkGte(JVar memory v, Context memory ctx, uint256 threshold) internal pure returns (bool) {
        uint256 cur = toUint(v.value);
        if (cur >= threshold) {
            _addGuard(ctx, v, OP_GTE, threshold, STRATEGY_RELAXED);
            return true;
        } else {
            _addGuard(ctx, v, OP_LT, threshold, STRATEGY_RELAXED);
            return false;
        }
    }

    function checkGt(JVar memory v, Context memory ctx, uint256 threshold) internal pure returns (bool) {
        uint256 cur = toUint(v.value);
        if (cur > threshold) {
            _addGuard(ctx, v, OP_GT, threshold, STRATEGY_RELAXED);
            return true;
        } else {
            _addGuard(ctx, v, OP_LTE, threshold, STRATEGY_RELAXED);
            return false;
        }
    }

    function checkEq(JVar memory v, Context memory ctx, uint256 target) internal pure returns (bool) {
        uint256 cur = toUint(v.value);
        if (cur == target) {
            _addGuard(ctx, v, OP_EQ, target, STRATEGY_STRICT);
            return true;
        } else {
            _addGuard(ctx, v, OP_NEQ, target, STRATEGY_STRICT);
            return false;
        }
    }

    // Optional "must pass" wrappers (assert style)
    function assertGte(JVar memory v, Context memory ctx, uint256 threshold) internal pure {
        require(checkGte(v, ctx, threshold), "JOYUE: assertGte failed");
    }

    // ============================================================
    //                      Deltas
    // ============================================================

    function _pushDelta(Context memory ctx, Delta memory d) private pure {
        Delta[] memory oldD = ctx.deltas;
        Delta[] memory newD = new Delta[](oldD.length + 1);
        for (uint256 i = 0; i < oldD.length; i++) {
            newD[i] = oldD[i];
        }
        newD[oldD.length] = d;
        ctx.deltas = newD;
    }

    function _addDelta(Context memory ctx, JVar memory v, uint8 op, bytes memory val) private pure {
        _pushDelta(ctx, Delta({contractAddr: v.contractAddr, key: v.key, op: op, val: val}));
        _applyCacheWrite(ctx, v, op, val);
    }

    function add(JVar memory v, Context memory ctx, uint256 amount) internal pure {
        _addDelta(ctx, v, D_ADD, abi.encode(amount));
    }

    function sub(JVar memory v, Context memory ctx, uint256 amount) internal pure {
        _addDelta(ctx, v, D_SUB, abi.encode(amount));
    }

    function set(JVar memory v, Context memory ctx, uint256 value) internal pure {
        _addDelta(ctx, v, D_SET, abi.encode(value));
    }

    // ============================================================
    //                      ExprBuilder (RPN)
    // ============================================================

    function expr(Context memory) internal pure returns (ExprBuilder memory b) {
        return b;
    }

    function _pushToken(ExprBuilder memory b, bytes memory tok) private pure returns (ExprBuilder memory) {
        bytes[] memory oldT = b.tokens;
        bytes[] memory newT = new bytes[](oldT.length + 1);
        for (uint256 i = 0; i < oldT.length; i++) newT[i] = oldT[i];
        newT[oldT.length] = tok;
        b.tokens = newT;
        return b;
    }

    function _pushEval(ExprBuilder memory b, uint256 v) private pure returns (ExprBuilder memory) {
        uint256[] memory oldS = b.evalStack;
        uint256[] memory newS = new uint256[](oldS.length + 1);
        for (uint256 i = 0; i < oldS.length; i++) newS[i] = oldS[i];
        newS[oldS.length] = v;
        b.evalStack = newS;
        return b;
    }

    function pushVar(ExprBuilder memory b, JVar memory v) internal pure returns (ExprBuilder memory) {
        // token: [TOK_VAR | addr(20) | key(32) | ver(8)]
        bytes memory t = abi.encodePacked(TOK_VAR, v.contractAddr, v.key, v.version);
        b = _pushToken(b, t);
        b = _pushEval(b, toUint(v.value));
        return b;
    }

    function pushArg(ExprBuilder memory b, uint8 argIndex, uint256 argValue) internal pure returns (ExprBuilder memory) {
        // NOTE: Solidity can't access raw args automatically; caller supplies argValue for evaluation.
        // TODO: standardize arg plumbing for automatic decoding by codegen/macro layer.
        bytes memory t = abi.encodePacked(TOK_ARG, argIndex);
        b = _pushToken(b, t);
        b = _pushEval(b, argValue);
        return b;
    }

    function pushConst(ExprBuilder memory b, uint256 c) internal pure returns (ExprBuilder memory) {
        bytes memory t = abi.encodePacked(TOK_CONST, c);
        b = _pushToken(b, t);
        b = _pushEval(b, c);
        return b;
    }

    function _binOp(ExprBuilder memory b, uint8 op) private pure returns (ExprBuilder memory) {
        require(b.evalStack.length >= 2, "JOYUE: expr stack underflow");
        uint256 len = b.evalStack.length;
        uint256 r = b.evalStack[len - 1];
        uint256 l = b.evalStack[len - 2];

        uint256 out;
        if (op == EXPR_ADD) out = l + r;
        else if (op == EXPR_SUB) out = l - r;
        else if (op == EXPR_MUL) out = l * r;
        else if (op == EXPR_DIV) out = (r == 0) ? 0 : (l / r);
        else revert("JOYUE: bad expr op");

        // stack: pop2 push1 => new length = len - 1
        uint256[] memory newS = new uint256[](len - 1);
        for (uint256 i = 0; i < len - 2; i++) newS[i] = b.evalStack[i];
        newS[len - 2] = out;
        b.evalStack = newS;

        b = _pushToken(b, abi.encodePacked(TOK_OP, op));
        return b;
    }

    function opAdd(ExprBuilder memory b) internal pure returns (ExprBuilder memory) {
        return _binOp(b, EXPR_ADD);
    }

    function opMul(ExprBuilder memory b) internal pure returns (ExprBuilder memory) {
        return _binOp(b, EXPR_MUL);
    }

    function checkExprGt(ExprBuilder memory b, Context memory ctx, uint256 threshold) internal pure returns (bool) {
        require(b.evalStack.length == 1, "JOYUE: expr not reduced");
        bool ok = b.evalStack[0] > threshold;

        // OP_EXPR payload: abi.encode(tokens, cmpOp, threshold)
        uint8 cmp = ok ? OP_GT : OP_LTE;
        _pushGuard(
            ctx,
            Guard({
                contractAddr: ctx.req.targetAddr, // logical owner; execution engine will parse tokens for real routing
                key: bytes32(0),                  // unused for expr; routing inside tokens
                readVersion: 0,
                strategy: STRATEGY_RELAXED,
                op: OP_EXPR,
                val: abi.encode(b.tokens, cmp, threshold)
            })
        );
        return ok;
    }

    // ============================================================
    //                      Cache fast-forward (optional)
    // ============================================================
    function _applyCacheWrite(Context memory ctx, JVar memory v, uint8 op, bytes memory val) private pure {
        if (ctx.cacheAddr == address(0)) return;

        uint256 cur = toUint(v.value);
        uint256 newVal = cur;
        if (op == D_ADD) {
            newVal = cur + abi.decode(val, (uint256));
        } else if (op == D_SUB) {
            uint256 x = abi.decode(val, (uint256));
            if (cur < x) return;
            newVal = cur - x;
        } else if (op == D_SET) {
            newVal = abi.decode(val, (uint256));
        } else if (op == D_BIT_OR) {
            newVal = cur | abi.decode(val, (uint256));
        } else if (op == D_BIT_CLEAR) {
            newVal = cur & ~abi.decode(val, (uint256));
        } else if (op == D_BIT_XOR) {
            newVal = cur ^ abi.decode(val, (uint256));
        } else {
            return;
        }

        try IJoyueCacheWriter(ctx.cacheAddr).setUint(v.contractAddr, v.key, newVal, v.version + 1) {
            v.value = abi.encode(newVal);
            v.version = v.version + 1;
        } catch {
            // ignore write failures
        }
    }
}


