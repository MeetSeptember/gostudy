// SPDX-License-Identifier: MIT
pragma solidity >=0.4.22;

import "./JoyueLib.sol";
import "./JoyueVerifier.sol";

/**
 * @title JoyueCoordinator
 * @dev 流程层：处理 Agent bundle、矩阵 2PC、参与者列冻结。
 */
contract JoyueCoordinator is JoyueVerifier {
    // ============================================================
    //      Participant-side column request (freeze / transient)
    // ============================================================
    struct ColumnGuard {
        uint32 guardIndex; // index in the *global* guard list of that transaction
        JoyueLib.Guard guard;
    }

    struct ColumnTx {
        bytes32 txHash;
        ColumnGuard[] guards;
        JoyueLib.Delta[] deltas;
    }

    struct ColumnResult {
        bytes32 txHash;
        bool ok;
        bool guardFailed;
        uint32 failedGuardIndex;
        uint32 failedDeltaIndex;
        bytes32 latestKey;
        uint256 latestVal;
        uint64 latestVer;
    }

    // ============================================================
    //      Coordinator-side bundle processing (MasterA entry)
    // ============================================================
    enum FinalStatus {
        FINAL_COMMIT,
        FINAL_FAIL
    }

    struct IntentDetail {
        bytes32 txHash;
        address sender;
        uint64 nonce;
        JoyueLib.RawRequest req;
        JoyueLib.Guard[] guards;
        JoyueLib.Delta[] deltas;
    }

    struct IntentBundle {
        bytes32 bundleId;
        uint32 agentShardId;
        uint32 masterShardId;
        address agentContract;
        address masterContract;
        uint64 epoch;
        uint64 agentBlockNumber;
        uint64 timestampMs;
        IntentDetail[] detail;
    }

    struct FinalResult {
        bytes32 txHash;
        FinalStatus status;
        uint8 attemptsUsed; // 1 or 2
    }

    struct PrecheckCtx {
        bool[] active;
        bool[] useOverride;
        JoyueLib.Guard[][] guardsOverride;
        JoyueLib.Delta[][] deltasOverride;
        bool[] terminalFail;
    }

    struct AttemptResult {
        bool[] committed;
        bool[] needRetry;
        bool[] callbackFail;
    }

    struct Decision {
        bytes32[] commitTx;
        bytes32[] retryTx;
        bytes32[] failTx;
        uint256 commitCount;
        uint256 retryCount;
        uint256 failCount;
    }

    struct RetryPrep {
        bool[] active;
        bool[] useOverride;
        JoyueLib.Guard[][] guardsOverride;
        JoyueLib.Delta[][] deltasOverride;
        bool[] terminalFail;
    }

    event RoundDone(bytes32 batchId, uint8 attempt, uint256 commitCount, uint256 retryOrFailCount);
    event Finalized(bytes32 txHash, FinalStatus status, uint8 attemptsUsed);

    // ============================================================
    //   Participant APIs (Scatter phase): verify + transient freeze
    // ============================================================
    function batchVerifyAndFreeze(bytes32 batchId, ColumnTx[] calldata txs)
        external
        returns (ColumnResult[] memory results)
    {
        results = new ColumnResult[](txs.length);
        for (uint256 i = 0; i < txs.length; i++) {
            ColumnTx calldata t = txs[i];
            ColumnResult memory r;
            r.txHash = t.txHash;

            // 1) verify guards
            bool guardOk = true;
            for (uint256 gi = 0; gi < t.guards.length; gi++) {
                JoyueLib.Guard calldata g = t.guards[gi].guard;
                if (!_verifyAtomicGuard(g)) {
                    guardOk = false;
                    r.ok = false;
                    r.guardFailed = true;
                    r.failedGuardIndex = t.guards[gi].guardIndex;

                    r.latestKey = g.key;
                    r.latestVal = _u[g.key];
                    r.latestVer = _ver[g.key];
                    break;
                }
            }

            if (!guardOk) {
                results[i] = r;
                continue;
            }

            // 2) freeze deltas into transient state
            bool deltaOk = true;
            for (uint256 di = 0; di < t.deltas.length; di++) {
                JoyueLib.Delta calldata d = t.deltas[di];
                if (d.contractAddr != address(this)) {
                    deltaOk = false;
                    r.failedDeltaIndex = uint32(di);
                    break;
                }
                uint256 curT = _getTransient(batchId, d.key);

                if (d.op == JoyueLib.D_ADD) {
                    uint256 x = abi.decode(d.val, (uint256));
                    _setTransient(batchId, d.key, curT + x);
                } else if (d.op == JoyueLib.D_SUB) {
                    uint256 x = abi.decode(d.val, (uint256));
                    if (curT < x) {
                        deltaOk = false;
                        r.failedDeltaIndex = uint32(di);
                        r.latestKey = d.key;
                        r.latestVal = curT;
                        r.latestVer = _ver[d.key];
                        break;
                    }
                    _setTransient(batchId, d.key, curT - x);
                } else if (d.op == JoyueLib.D_SET) {
                    uint256 x = abi.decode(d.val, (uint256));
                    _setTransient(batchId, d.key, x);
                } else if (d.op == JoyueLib.D_BIT_OR) {
                    uint256 x = abi.decode(d.val, (uint256));
                    _setTransient(batchId, d.key, curT | x);
                } else if (d.op == JoyueLib.D_BIT_CLEAR) {
                    uint256 x = abi.decode(d.val, (uint256));
                    _setTransient(batchId, d.key, curT & ~x);
                } else if (d.op == JoyueLib.D_BIT_XOR) {
                    uint256 x = abi.decode(d.val, (uint256));
                    _setTransient(batchId, d.key, curT ^ x);
                } else {
                    deltaOk = false;
                    r.failedDeltaIndex = uint32(di);
                    break;
                }
            }

            if (!deltaOk) {
                r.ok = false;
                r.guardFailed = false;
                results[i] = r;
                continue;
            }

            // 3) mark frozen success：保存冻结，并对 SUB 做“真实减库存”占用
            _applyAuthoritativeFreeze(t.deltas);
            _frozen[batchId][t.txHash] = true;
            _frozenDeltas[batchId][t.txHash] = abi.encode(t.deltas);

            r.ok = true;
            results[i] = r;
        }
        return results;
    }

    /**
     * @dev Finalize a round for a subset of txs.
     */
    function finalizeBatch(bytes32 batchId, bytes32[] calldata txHashes, bool commit) external {
        for (uint256 i = 0; i < txHashes.length; i++) {
            bytes32 h = txHashes[i];
            if (!_frozen[batchId][h]) continue;

            if (commit) {
                _finalizeCommitApply(abi.decode(_frozenDeltas[batchId][h], (JoyueLib.Delta[])));
            } else {
                JoyueLib.Delta[] memory dsRollback = abi.decode(_frozenDeltas[batchId][h], (JoyueLib.Delta[]));
                for (uint256 j = 0; j < dsRollback.length; j++) {
                    JoyueLib.Delta memory d = dsRollback[j];
                    if (d.contractAddr != address(this)) continue;
                    if (d.op == JoyueLib.D_SUB) {
                        uint256 x = abi.decode(d.val, (uint256));
                        _setUint(d.key, _u[d.key] + x);
                        if (_tSet[batchId][d.key]) {
                            _tU[batchId][d.key] += x;
                        }
                    }
                }
            }

            delete _frozen[batchId][h];
            delete _frozenDeltas[batchId][h];
        }
    }

    function _finalizeCommitApply(JoyueLib.Delta[] memory ds) internal {
        for (uint256 j = 0; j < ds.length; j++) {
            if (ds[j].op == JoyueLib.D_SUB && ds[j].contractAddr == address(this)) continue;
            require(_applyDeltaMem(ds[j]), "JOYUE: commit apply failed");
        }
    }

    function clearRound(bytes32 batchId) external {
        bytes32[] storage keys = _tKeys[batchId];
        for (uint256 k = 0; k < keys.length; k++) {
            bytes32 key = keys[k];
            delete _tU[batchId][key];
            delete _tSet[batchId][key];
            delete _tKeySeen[batchId][key];
        }
        delete _tKeys[batchId];
    }

    // ============================================================
    //      Coordinator API: process Agent bundle with 1 retry
    // ============================================================
    function processBundleWithOneRetry(
        IntentBundle calldata bundle,
        uint64 roundIdBase,
        address[] calldata participants
    ) external returns (FinalResult[] memory finals) {
        require(participants.length > 0, "JOYUE: participants empty");
        finals = new FinalResult[](bundle.detail.length);
        bool[] memory done = new bool[](bundle.detail.length);

        PrecheckCtx memory pre = _precheckGuardsAndMaybeRetry(bundle.detail, done);
        _finalizeFailZeroRound(bundle.detail, pre.terminalFail, finals, done);

        AttemptResult memory att1 = _processFirstAttempt(bundle, roundIdBase, participants, pre, finals, done);
        if (_allDone(done)) return finals;
        _processSecondAttempt(bundle, roundIdBase, participants, pre, att1, finals, done);
        return finals;
    }

    function _processFirstAttempt(
        IntentBundle calldata bundle,
        uint64 roundIdBase,
        address[] calldata participants,
        PrecheckCtx memory pre,
        FinalResult[] memory finals,
        bool[] memory done
    ) internal returns (AttemptResult memory att1) {
        bytes32 batch1 = _batchId(bundle.epoch, roundIdBase, 1);
        att1 = _runAttempt(
            batch1,
            1,
            participants,
            bundle.detail,
            done,
            pre.active,
            pre.useOverride,
            pre.guardsOverride,
            pre.deltasOverride
        );
        _finalizeCommitRound(bundle.detail, att1.committed, finals, done, 1);
    }

    function _processSecondAttempt(
        IntentBundle calldata bundle,
        uint64 roundIdBase,
        address[] calldata participants,
        PrecheckCtx memory pre,
        AttemptResult memory att1,
        FinalResult[] memory finals,
        bool[] memory done
    ) internal {
        RetryPrep memory prep2 = _prepareRetryRound(bundle.detail, done, att1.needRetry, pre.guardsOverride, pre.deltasOverride);
        _finalizeFailRetryRound(bundle.detail, prep2.terminalFail, finals, done);
        bytes32 batch2 = _batchId(bundle.epoch, roundIdBase, 2);
        AttemptResult memory att2 = _runAttempt(
            batch2,
            2,
            participants,
            bundle.detail,
            done,
            prep2.active,
            prep2.useOverride,
            prep2.guardsOverride,
            prep2.deltasOverride
        );
        _finalizeCommitOrFailRound(bundle.detail, att2.committed, att2.callbackFail, finals, done, 2);
    }

    function _batchId(uint64 epoch, uint64 roundIdBase, uint8 attempt) internal view returns (bytes32) {
        return keccak256(abi.encodePacked(epoch, address(this), roundIdBase, attempt));
    }

    function _runAttempt(
        bytes32 batchId,
        uint8 attemptNo,
        address[] calldata participants,
        IntentDetail[] calldata details,
        bool[] memory done,
        bool[] memory active,
        bool[] memory useOverride,
        JoyueLib.Guard[][] memory guardsOverride,
        JoyueLib.Delta[][] memory deltasOverride
    ) internal returns (AttemptResult memory ar) {
        bool[][] memory okMatrix = _scatterAndFreeze(
            batchId,
            participants,
            details,
            done,
            active,
            useOverride,
            guardsOverride,
            deltasOverride
        );
        (Decision memory dec, AttemptResult memory out) = _decide(attemptNo, participants.length, details, done, active, okMatrix);

        // todo 这里需要考虑是否需要在这里实现一次跨分片请求，在第二次确认请求的时候将这些内容附带过去
        _finalizeColumns(batchId, participants, dec);
        emit RoundDone(batchId, attemptNo, dec.commitCount, dec.retryCount + dec.failCount);
        return out;
    }

    function _scatterAndFreeze(
        bytes32 batchId,
        address[] calldata participants,
        IntentDetail[] calldata details,
        bool[] memory done,
        bool[] memory active,
        bool[] memory useOverride,
        JoyueLib.Guard[][] memory guardsOverride,
        JoyueLib.Delta[][] memory deltasOverride
    ) internal returns (bool[][] memory okMatrix) {
        okMatrix = new bool[][](participants.length);
        for (uint256 p = 0; p < participants.length; p++) {
            okMatrix[p] = new bool[](details.length);
            for (uint256 i = 0; i < details.length; i++) okMatrix[p][i] = true;

            _scatterOneParticipant(
                batchId,
                participants[p],
                details,
                done,
                active,
                useOverride,
                guardsOverride,
                deltasOverride,
                okMatrix[p]
            );
        }
    }

    function _scatterOneParticipant(
        bytes32 batchId,
        address participant,
        IntentDetail[] calldata details,
        bool[] memory done,
        bool[] memory active,
        bool[] memory useOverride,
        JoyueLib.Guard[][] memory guardsOverride,
        JoyueLib.Delta[][] memory deltasOverride,
        bool[] memory okRow
    ) internal {
        (ColumnTx[] memory col, uint256[] memory idxMap) = _buildColumnTxsCompact(
            participant,
            details,
            done,
            active,
            useOverride,
            guardsOverride,
            deltasOverride
        );

        // todo 这里需要调整为真实的合约调用，可以利用shardcall，但是需要适应性改造
        ColumnResult[] memory res = JoyueCoordinator(participant).batchVerifyAndFreeze(batchId, col);
        require(res.length == idxMap.length, "JOYUE: res/idxMap mismatch");
        for (uint256 k = 0; k < res.length; k++) {
            okRow[idxMap[k]] = res[k].ok;
        }
    }

    function _decide(
        uint8 attemptNo,
        uint256 participantCount,
        IntentDetail[] calldata details,
        bool[] memory done,
        bool[] memory active,
        bool[][] memory okMatrix
    ) internal pure returns (Decision memory dec, AttemptResult memory out) {
        dec.commitTx = new bytes32[](details.length);
        dec.retryTx = new bytes32[](details.length);
        dec.failTx = new bytes32[](details.length);

        out.committed = new bool[](details.length);
        out.needRetry = new bool[](details.length);
        out.callbackFail = new bool[](details.length);

        for (uint256 i = 0; i < details.length; i++) {
            if (done[i]) continue;
            if (!active[i]) continue;

            bool okAll = true;
            for (uint256 p = 0; p < participantCount; p++) {
                if (!okMatrix[p][i]) {
                    okAll = false;
                    break;
                }
            }

            if (okAll) {
                out.committed[i] = true;
                dec.commitTx[dec.commitCount++] = details[i].txHash;
            } else if (attemptNo == 1) {
                out.needRetry[i] = true;
                dec.retryTx[dec.retryCount++] = details[i].txHash;
            } else {
                out.callbackFail[i] = true;
                dec.failTx[dec.failCount++] = details[i].txHash;
            }
        }
        return (dec, out);
    }

    function _finalizeColumns(bytes32 batchId, address[] calldata participants, Decision memory dec) internal {
        bytes32[] memory commits = _slice(dec.commitTx, dec.commitCount);
        bytes32[] memory retries = _slice(dec.retryTx, dec.retryCount);
        bytes32[] memory fails = _slice(dec.failTx, dec.failCount);

        for (uint256 p = 0; p < participants.length; p++) {
            if (dec.commitCount > 0) JoyueCoordinator(participants[p]).finalizeBatch(batchId, commits, true);
            if (dec.retryCount > 0) JoyueCoordinator(participants[p]).finalizeBatch(batchId, retries, false);
            if (dec.failCount > 0) JoyueCoordinator(participants[p]).finalizeBatch(batchId, fails, false);
            JoyueCoordinator(participants[p]).clearRound(batchId);
        }
    }

    function _precheckGuardsAndMaybeRetry(IntentDetail[] calldata details, bool[] memory done)
        internal
        returns (PrecheckCtx memory ctx)
    {
        //预先获取相关的合约状态，需要用RPC获取当前批次最权威的状态
        _prefetchBegin(details);

        ctx.active = new bool[](details.length);
        ctx.useOverride = new bool[](details.length);
        ctx.guardsOverride = new JoyueLib.Guard[][](details.length);
        ctx.deltasOverride = new JoyueLib.Delta[][](details.length);
        ctx.terminalFail = new bool[](details.length);

        for (uint256 i = 0; i < details.length; i++) {
            if (done[i]) {
                ctx.active[i] = false;
                continue;
            }

            bool hasFailure = false;

            for (uint256 gi = 0; gi < details[i].guards.length; gi++) {
                JoyueLib.Guard calldata g = details[i].guards[gi];
                (
                    bool guardOk,
                    uint256 cur,
                    uint64 curVer,
                    bool retryOk,
                    JoyueLib.Guard[] memory newGuards,
                    JoyueLib.Delta[] memory newDeltas
                ) = _verifyGuardWithPrefetch(g, details[i].req, uint32(gi));

                if (guardOk) {
                    continue;
                }

                if (retryOk) {
                    ctx.useOverride[i] = true;
                    ctx.guardsOverride[i] = newGuards;
                    ctx.deltasOverride[i] = newDeltas;
                    ctx.active[i] = true;
                } else {
                    // 标记为真正的失败
                    hasFailure = true;
                    ctx.terminalFail[i] = true;
                    done[i] = true;
                    ctx.active[i] = false;
                }
                // 无论重试是否成功，旧的 Guard 链都不再继续验证了
                break;
            }

            if (!hasFailure && !ctx.terminalFail[i]) {
                ctx.active[i] = true;
                // Guard 全部通过，应用原意图的 deltas 到预取缓存，供后续交易判断
                _prefetchApplyDeltas(_selectDeltas(ctx.useOverride[i], details[i].deltas, ctx.deltasOverride[i]));
            }
        }

        _prefetchEnd();
    }

    // ---------- Prefetch 批量入口/清理（基于 IntentDetail） ----------
    function _prefetchBegin(IntentDetail[] calldata details) internal {
        for (uint256 i = 0; i < details.length; i++) {
            for (uint256 g = 0; g < details[i].guards.length; g++) {
                JoyueLib.Guard calldata guard = details[i].guards[g];
                _prefetchLoad(guard.contractAddr, guard.key);
            }
            for (uint256 d = 0; d < details[i].deltas.length; d++) {
                JoyueLib.Delta calldata delta = details[i].deltas[d];
                _prefetchLoad(delta.contractAddr, delta.key);
            }
        }
    }

    function _prefetchEnd() internal {
        for (uint256 i = 0; i < _prefetchKeys.length; i++) {
            delete _prefetch[_prefetchKeys[i]];
        }
        delete _prefetchKeys;
    }

    function _selectDeltas(
        bool useOverride,
        JoyueLib.Delta[] calldata deltasOrig,
        JoyueLib.Delta[] memory deltasOverride
    ) internal pure returns (JoyueLib.Delta[] memory out) {
        if (useOverride) return deltasOverride;
        return deltasOrig;
    }

    function _finalizeFailZeroRound(
        IntentDetail[] calldata details,
        bool[] memory terminalFail0,
        FinalResult[] memory finals,
        bool[] memory done
    ) internal {
        for (uint256 i = 0; i < details.length; i++) {
            if (terminalFail0[i]) {
                finals[i] = FinalResult({txHash: details[i].txHash, status: FinalStatus.FINAL_FAIL, attemptsUsed: 0});
                done[i] = true;
                emit Finalized(details[i].txHash, FinalStatus.FINAL_FAIL, 0);
            }
        }
    }

    function _finalizeCommitRound(
        IntentDetail[] calldata details,
        bool[] memory committed,
        FinalResult[] memory finals,
        bool[] memory done,
        uint8 attemptUsed
    ) internal {
        for (uint256 i = 0; i < details.length; i++) {
            if (done[i]) continue;
            if (committed[i]) {
                finals[i] = FinalResult({txHash: details[i].txHash, status: FinalStatus.FINAL_COMMIT, attemptsUsed: attemptUsed});
                done[i] = true;
                emit Finalized(details[i].txHash, FinalStatus.FINAL_COMMIT, attemptUsed);
            }
        }
    }

    function _finalizeFailRetryRound(
        IntentDetail[] calldata details,
        bool[] memory terminalFailRetry,
        FinalResult[] memory finals,
        bool[] memory done
    ) internal {
        for (uint256 i = 0; i < details.length; i++) {
            if (done[i]) continue;
            if (!terminalFailRetry[i]) continue;
            finals[i] = FinalResult({txHash: details[i].txHash, status: FinalStatus.FINAL_FAIL, attemptsUsed: 2});
            done[i] = true;
            emit Finalized(details[i].txHash, FinalStatus.FINAL_FAIL, 2);
        }
    }

    function _prepareRetryRound(
        IntentDetail[] calldata details,
        bool[] memory done,
        bool[] memory retryMask,
        JoyueLib.Guard[][] memory guardsOverride1,
        JoyueLib.Delta[][] memory deltasOverride1
    ) internal view returns (RetryPrep memory prep) {
        prep.active = new bool[](details.length);
        prep.useOverride = new bool[](details.length);
        prep.guardsOverride = new JoyueLib.Guard[][](details.length);
        prep.deltasOverride = new JoyueLib.Delta[][](details.length);
        prep.terminalFail = new bool[](details.length);

        for (uint256 i = 0; i < details.length; i++) {
            if (done[i]) continue;
            if (!retryMask[i]) continue;


            // todo 这里的逻辑树重试 已经是最新的资源
            (bool logicTreeOk, JoyueLib.Guard[] memory newG, JoyueLib.Delta[] memory newD) =
                _logicTreeRetry(details[i].req, 0, bytes32(0), 0, 0);

            if (!logicTreeOk) {
                prep.terminalFail[i] = true;
                continue;
            }

            prep.active[i] = true;
            prep.useOverride[i] = true;
            prep.guardsOverride[i] = newG;
            prep.deltasOverride[i] = newD;
        }
    }

    function _finalizeCommitOrFailRound(
        IntentDetail[] calldata details,
        bool[] memory committed,
        bool[] memory fail,
        FinalResult[] memory finals,
        bool[] memory done,
        uint8 attemptUsed
    ) internal {
        for (uint256 i = 0; i < details.length; i++) {
            if (done[i]) continue;
            if (committed[i]) {
                finals[i] = FinalResult({txHash: details[i].txHash, status: FinalStatus.FINAL_COMMIT, attemptsUsed: attemptUsed});
                emit Finalized(details[i].txHash, FinalStatus.FINAL_COMMIT, attemptUsed);
            } else if (fail[i]) {
                finals[i] = FinalResult({txHash: details[i].txHash, status: FinalStatus.FINAL_FAIL, attemptsUsed: attemptUsed});
                emit Finalized(details[i].txHash, FinalStatus.FINAL_FAIL, attemptUsed);
            } else {
                finals[i] = FinalResult({txHash: details[i].txHash, status: FinalStatus.FINAL_FAIL, attemptsUsed: attemptUsed});
                emit Finalized(details[i].txHash, FinalStatus.FINAL_FAIL, attemptUsed);
            }
            done[i] = true;
        }
    }

    function _buildColumnTxsCompact(
        address participant,
        IntentDetail[] calldata details,
        bool[] memory done,
        bool[] memory active,
        bool[] memory useOverride,
        JoyueLib.Guard[][] memory guardsOverride,
        JoyueLib.Delta[][] memory deltasOverride
    ) internal pure returns (ColumnTx[] memory col, uint256[] memory idxMap) {
        uint256 cnt = 0;
        for (uint256 i = 0; i < details.length; i++) {
            if (!done[i] && active[i]) cnt++;
        }
        col = new ColumnTx[](cnt);
        idxMap = new uint256[](cnt);

        uint256 ptr = 0;
        for (uint256 i = 0; i < details.length; i++) {
            if (done[i] || !active[i]) continue;
            ColumnTx memory t = _buildSingleColumnTx(
                participant,
                details[i],
                useOverride[i],
                guardsOverride[i],
                deltasOverride[i]
            );
            col[ptr] = t;
            idxMap[ptr] = i;
            ptr++;
        }
        return (col, idxMap);
    }

    function _buildSingleColumnTx(
        address participant,
        IntentDetail calldata detail,
        bool useOverride,
        JoyueLib.Guard[] memory guardsOverride,
        JoyueLib.Delta[] memory deltasOverride
    ) internal pure returns (ColumnTx memory t) {
        // 计算 guard 数量
        uint256 gCount = 0;
        if (useOverride) {
            for (uint256 gi = 0; gi < guardsOverride.length; gi++) {
                if (guardsOverride[gi].contractAddr == participant) gCount++;
            }
        } else {
            JoyueLib.Guard[] calldata gsrcCd = detail.guards;
            for (uint256 gi = 0; gi < gsrcCd.length; gi++) {
                if (gsrcCd[gi].contractAddr == participant) gCount++;
            }
        }

        // 计算 delta 数量
        uint256 dCount = 0;
        if (useOverride) {
            for (uint256 di = 0; di < deltasOverride.length; di++) {
                if (deltasOverride[di].contractAddr == participant) dCount++;
            }
        } else {
            JoyueLib.Delta[] calldata dsrcCd = detail.deltas;
            for (uint256 di = 0; di < dsrcCd.length; di++) {
                if (dsrcCd[di].contractAddr == participant) dCount++;
            }
        }

        // 填充 guards
        ColumnGuard[] memory gs = new ColumnGuard[](gCount);
        uint256 gx = 0;
        if (useOverride) {
            for (uint256 gi = 0; gi < guardsOverride.length; gi++) {
                JoyueLib.Guard memory g = guardsOverride[gi];
                if (g.contractAddr == participant) {
                    gs[gx] = ColumnGuard({guardIndex: uint32(gi), guard: g});
                    gx++;
                }
            }
        } else {
            JoyueLib.Guard[] calldata gsrcCd2 = detail.guards;
            for (uint256 gi = 0; gi < gsrcCd2.length; gi++) {
                JoyueLib.Guard calldata g = gsrcCd2[gi];
                if (g.contractAddr == participant) {
                    gs[gx] = ColumnGuard({guardIndex: uint32(gi), guard: g});
                    gx++;
                }
            }
        }

        // 填充 deltas
        JoyueLib.Delta[] memory ds = new JoyueLib.Delta[](dCount);
        uint256 dx = 0;
        if (useOverride) {
            for (uint256 di = 0; di < deltasOverride.length; di++) {
                JoyueLib.Delta memory d = deltasOverride[di];
                if (d.contractAddr == participant) {
                    ds[dx] = d;
                    dx++;
                }
            }
        } else {
            JoyueLib.Delta[] calldata dsrcCd2 = detail.deltas;
            for (uint256 di = 0; di < dsrcCd2.length; di++) {
                JoyueLib.Delta calldata d = dsrcCd2[di];
                if (d.contractAddr == participant) {
                    ds[dx] = d;
                    dx++;
                }
            }
        }

        t.txHash = detail.txHash;
        t.guards = gs;
        t.deltas = ds;
        return t;
    }

    function _slice(bytes32[] memory arr, uint256 n) internal pure returns (bytes32[] memory out) {
        out = new bytes32[](n);
        for (uint256 i = 0; i < n; i++) out[i] = arr[i];
    }

    function _allDone(bool[] memory done) internal pure returns (bool) {
        for (uint256 i = 0; i < done.length; i++) {
            if (!done[i]) return false;
        }
        return true;
    }
}

