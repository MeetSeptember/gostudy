// SPDX-License-Identifier: MIT
pragma solidity ^0.8.0;

import "./JoyueLib.sol";
import "./JoyueRpcOracleMock.sol";
import "./JoyueStorage.sol";

/**
 * @title JoyueVerifier
 * @dev 逻辑层：Guard 校验、LogicTree 占位、Delta 应用。
 */
contract JoyueVerifier is JoyueStorage {
    event StateBroadcast(address indexed contractAddr, bytes32 indexed key, uint256 value, uint64 version);

    // Prefetch 缓存（仅本次调用范围），key=keccak256(contractAddr,key)
    struct PrefetchEntry {
        uint256 val;
        uint64 ver;
        bool has;
    }
    mapping(bytes32 => PrefetchEntry) internal _prefetch;
    bytes32[] internal _prefetchKeys;

    // -------- Guard 比较 ----------
    function _cmp(uint8 op, uint256 cur, uint256 rhs) internal pure returns (bool) {
        if (op == 0x01) return cur == rhs;      // EQ
        if (op == 0x02) return cur != rhs;      // NEQ
        if (op == 0x03) return cur > rhs;       // GT
        if (op == 0x04) return cur >= rhs;      // GTE
        if (op == 0x05) return cur < rhs;       // LT
        if (op == 0x06) return cur <= rhs;      // LTE
        return false;
    }

    function _verifyAtomicGuard(JoyueLib.Guard calldata g) internal view returns (bool) {
        if (g.contractAddr != address(this)) return false;
        if (g.op == JoyueLib.OP_EXPR) {
            // 原子校验阶段复用表达式求值；无 RawRequest，可传空，TOK_ARG 将失败
            (bool ok, ) = _evalExprGuard(g, JoyueLib.RawRequest({targetAddr: address(0), selector: bytes4(0), args: ""}));
            return ok;
        }

        (uint256 cur, uint64 curVer) = getUint(g.key);

        // version strategy
        if (g.strategy == JoyueLib.STRATEGY_STRICT) {
            if (curVer != g.readVersion) return false;
        } else if (g.strategy == JoyueLib.STRATEGY_RELAXED) {
            if (curVer < g.readVersion) return false;
        } else {
            return false;
        }

        uint256 rhs = abi.decode(g.val, (uint256));
        return _cmp(g.op, cur, rhs);
    }

    /**
     * @dev 协调者侧 Guard 预检 + 逻辑树重试（仅在失败时尝试重算意图）。
     */
    function _verifyGuardWithPrefetch(
        JoyueLib.Guard calldata g,
        JoyueLib.RawRequest calldata req,
        uint32 guardIndex
    )
        internal
        view
        returns (
            bool guardOk,
            uint256 cur,
            uint64 curVer,
            bool retryOk,
            JoyueLib.Guard[] memory newGuards,
            JoyueLib.Delta[] memory newDeltas
        )
    {

        if (g.op == JoyueLib.OP_EXPR) {
            uint256 exprVal;
            (guardOk, exprVal) = _evalExprGuard(g, req);
            if (guardOk) {
                return (true, exprVal, 0, false, new JoyueLib.Guard[](0), new JoyueLib.Delta[](0));
            }
            // 表达式 guard 失败，尝试逻辑树重试（latestKey 传入 g.key 以保持上下文）
            (retryOk, newGuards, newDeltas) = _logicTreeRetry(req, guardIndex, g.key, exprVal, 0);
            return (guardOk, exprVal, 0, retryOk, newGuards, newDeltas);
        }

        (cur, curVer) = _prefetchRead(g.contractAddr, g.key);

        uint256 rhs = abi.decode(g.val, (uint256));
        guardOk = _cmp(g.op, cur, rhs);
        if (guardOk) {
            return (true, cur, curVer, false, new JoyueLib.Guard[](0), new JoyueLib.Delta[](0));
        }

        (retryOk, newGuards, newDeltas) = _logicTreeRetry(req, guardIndex, g.key, cur, curVer);
        return (guardOk, cur, curVer, retryOk, newGuards, newDeltas);
    }

    // -------- Delta 应用 ----------
    function _applyDeltaMem(JoyueLib.Delta memory d) internal returns (bool) {
        if (d.contractAddr != address(this)) return false;
        (uint256 cur, ) = getUint(d.key);
        bool applied = false;

        if (d.op == JoyueLib.D_ADD) {
            uint256 x = abi.decode(d.val, (uint256));
            _setUint(d.key, cur + x);
            applied = true;
        } else if (d.op == JoyueLib.D_SUB) {
            uint256 x = abi.decode(d.val, (uint256));
            if (cur < x) return false;
            _setUint(d.key, cur - x);
            applied = true;
        } else if (d.op == JoyueLib.D_SET) {
            uint256 x = abi.decode(d.val, (uint256));
            _setUint(d.key, x);
            applied = true;
        } else if (d.op == JoyueLib.D_BIT_OR) {
            uint256 x = abi.decode(d.val, (uint256));
            _setUint(d.key, cur | x);
            applied = true;
        } else if (d.op == JoyueLib.D_BIT_CLEAR) {
            uint256 x = abi.decode(d.val, (uint256));
            _setUint(d.key, cur & ~x);
            applied = true;
        } else if (d.op == JoyueLib.D_BIT_XOR) {
            uint256 x = abi.decode(d.val, (uint256));
            _setUint(d.key, cur ^ x);
            applied = true;
        }

        if (applied) {
            _broadcastState(d.key);
            return true;
        }
        return false;
    }

    function _applyDelta(JoyueLib.Delta calldata d) internal returns (bool) {
        JoyueLib.Delta memory dm = JoyueLib.Delta({contractAddr: d.contractAddr, key: d.key, op: d.op, val: d.val});
        return _applyDeltaMem(dm);
    }

    // ---------- Prefetch: 加载 / 读取 / 更新 ----------

    function _prefetchLoad(address c, bytes32 key) internal {
        bytes32 k = keccak256(abi.encodePacked(c, key));
        if (_prefetch[k].has) return;

        uint256 v;
        uint64 ver;
        if (c == address(this)) {
            (v, ver) = getUint(key);
            _prefetch[k] = PrefetchEntry({val: v, ver: ver, has: true});
        } else {
            require(rpcOracle != address(0), "JOYUE: rpcOracle not set");
            (uint256 v2, uint64 ver2, bool has) = JoyueRpcOracleMock(rpcOracle).getUint(c, key);
            if (has) {
                _prefetch[k] = PrefetchEntry({val: v2, ver: ver2, has: true});
            }
        }
        _prefetchKeys.push(k);
    }

    function _prefetchRead(address c, bytes32 key) internal view returns (uint256 val, uint64 ver) {
        bytes32 k = keccak256(abi.encodePacked(c, key));
        PrefetchEntry memory e = _prefetch[k];
        require(e.has, "JOYUE: prefetch missing");
        return (e.val, e.ver);
    }

    function _prefetchApplyDeltas(JoyueLib.Delta[] memory deltas) internal {
        for (uint256 i = 0; i < deltas.length; i++) {
            JoyueLib.Delta memory d = deltas[i];
            bytes32 k = keccak256(abi.encodePacked(d.contractAddr, d.key));
            PrefetchEntry memory e = _prefetch[k];
            if (!e.has) continue; // 若未预取到该键（不在本批次关注集合），跳过

            if (d.op == JoyueLib.D_ADD) {
                uint256 x = abi.decode(d.val, (uint256));
                e.val = e.val + x;
                e.ver += 1;
            } else if (d.op == JoyueLib.D_SUB) {
                uint256 x = abi.decode(d.val, (uint256));
                require(e.val >= x, "JOYUE: prefetch underflow");
                e.val = e.val - x;
                e.ver += 1;
            } else if (d.op == JoyueLib.D_SET) {
                uint256 x = abi.decode(d.val, (uint256));
                e.val = x;
                e.ver += 1;
            } else if (d.op == JoyueLib.D_BIT_OR) {
                uint256 x = abi.decode(d.val, (uint256));
                e.val = e.val | x;
                e.ver += 1;
            } else if (d.op == JoyueLib.D_BIT_CLEAR) {
                uint256 x = abi.decode(d.val, (uint256));
                e.val = e.val & ~x;
                e.ver += 1;
            } else if (d.op == JoyueLib.D_BIT_XOR) {
                uint256 x = abi.decode(d.val, (uint256));
                e.val = e.val ^ x;
                e.ver += 1;
            } else {
                // ignore unsupported
            }

            _prefetch[k] = e;
        }
    }

    function applyDeltas(JoyueLib.Delta[] calldata deltas) public returns (bool) {
        for (uint256 i = 0; i < deltas.length; i++) {
            if (!_applyDelta(deltas[i])) return false;
        }
        return true;
    }

    /**
     * @dev 冻结阶段对 SUB 做“真实占用”：权威状态先减库存，失败可在 finalize(rollback) 时加回。
     * 仅处理本合约的 D_SUB。
     */
    function _applyAuthoritativeFreeze(JoyueLib.Delta[] calldata deltas) internal {
        for (uint256 i = 0; i < deltas.length; i++) {
            JoyueLib.Delta calldata d = deltas[i];
            if (d.contractAddr != address(this)) continue;
            if (d.op == JoyueLib.D_SUB) {
                uint256 x = abi.decode(d.val, (uint256));
                (uint256 cur, ) = getUint(d.key);
                require(cur >= x, "JOYUE: freeze underflow");
                _setUint(d.key, cur - x);
            }
        }
    }

    // -------- LogicTree 占位 --------
    function _logicTreeRetry(
        JoyueLib.RawRequest calldata /*req*/,
        uint32 /*failedGuardIndex*/,
        bytes32 /*latestKey*/,
        uint256 /*latestVal*/,
        uint64 /*latestVer*/
    ) internal view returns (bool ok, JoyueLib.Guard[] memory newGuards, JoyueLib.Delta[] memory newDeltas) {
        ok = false;
        newGuards = new JoyueLib.Guard[](0);
        newDeltas = new JoyueLib.Delta[](0);
        return (ok, newGuards, newDeltas);
    }

    function _broadcastState(bytes32 key) internal {
        emit StateBroadcast(address(this), key, _u[key], _ver[key]);
    }

    // ============================================================
    //                  OP_EXPR (RPN) Guard 评估
    // ============================================================
    function _evalExprGuard(JoyueLib.Guard calldata g, JoyueLib.RawRequest calldata req)
        internal
        view
        returns (bool ok, uint256 valOut)
    {
        // guard.val: abi.encode(bytes[] tokens, uint8 cmp, uint256 threshold)
        (bytes[] memory tokens, uint8 cmp, uint256 threshold) = abi.decode(g.val, (bytes[], uint8, uint256));
        uint256[] memory stack = new uint256[](tokens.length);
        uint256 sp = 0;

        for (uint256 i = 0; i < tokens.length; i++) {
            bytes memory t = tokens[i];
            require(t.length >= 1, "JOYUE: bad expr token");
            uint8 kind = uint8(t[0]);

            if (kind == 0x01) {
                // TOK_VAR: [1 | addr(20) | key(32) | ver(8)]
                require(t.length >= 1 + 20 + 32 + 8, "JOYUE: bad TOK_VAR");
                address c;
                bytes32 k;
                uint64 ver;
                assembly {
                    c := shr(96, mload(add(t, 33)))       // bytes20 at offset 1
                    k := mload(add(t, 65))                // bytes32 at offset 21
                    ver := shr(192, mload(add(t, 73)))    // uint64 at offset 53
                }
                (uint256 vCur, ) = _prefetchRead(c, k);
                // 可选：版本校验，若需要可比较 ver 与预取版本；当前忽略 ver 仅做值比较
                stack[sp++] = vCur;
            } else if (kind == 0x02) {
                // TOK_ARG: 仅有 argIndex；当前缺少实参值，无法还原，直接失败
                return (false, 0);
            } else if (kind == 0x03) {
                // TOK_CONST: [1 | uint256]
                require(t.length >= 1 + 32, "JOYUE: bad TOK_CONST");
                uint256 cst;
                assembly {
                    cst := mload(add(t, 33))
                }
                stack[sp++] = cst;
            } else if (kind == 0x04) {
                // TOK_OP: [1 | opcode] where opcode in {ADD,SUB,MUL,DIV}
                require(t.length >= 2, "JOYUE: bad TOK_OP");
                require(sp >= 2, "JOYUE: expr stack underflow");
                uint256 b = stack[--sp];
                uint256 a = stack[--sp];
                uint8 op = uint8(t[1]);
                uint256 r;
                if (op == 0x01) {
                    r = a + b;
                } else if (op == 0x02) {
                    r = a - b;
                } else if (op == 0x03) {
                    r = a * b;
                } else if (op == 0x04) {
                    r = b == 0 ? 0 : a / b;
                } else {
                    return (false, 0);
                }
                stack[sp++] = r;
            } else {
                return (false, 0);
            }
        }

        require(sp == 1, "JOYUE: expr not reduced");
        valOut = stack[0];
        ok = _cmp(cmp, valOut, threshold);
        return (ok, valOut);
    }
}

