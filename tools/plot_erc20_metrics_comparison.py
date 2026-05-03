#!/usr/bin/env python3
"""
Compare Sparrow / Joyue / 2PC / Chainspace benchmark CSV + trigger JSONL (ERC20, AMM, NFT, or Bot):
  Trigger：优先读取 ``*-trigger.jsonl``，不存在时再读 ``*-trigger.json``（各 suite 相同）。
  - Stacked bar: success vs failure (by completion_type / success)
  - Latency: ECDF + percentile table (trigger.receipt_block_time − trigger.send_time, ms)
  - Joyue：以 trigger 的 shard_id 划分分片并分别统计（Joyue·0、Joyue·1…）；metrics 优先按 tx_id 对齐到该分片的 trigger
  - TPS 子图：Joyue 不按分片拆柱，整表合并一条「Joyue」柱。
  - TPS：各方案独立 L = mean(完成时间) − mean(trigger.send_time)（毫秒）；分母 L/1000 秒；
    分子为完成时间 ∈ [mean(send), mean(completion)] 闭区间的条数（与 L 一致）。

  Examples:
    python3 tools/plot_erc20_metrics_comparison.py --root . --suite erc20
    python3 tools/plot_erc20_metrics_comparison.py --root . --suite amm
    python3 tools/plot_erc20_metrics_comparison.py --root . --suite nft
    python3 tools/plot_erc20_metrics_comparison.py --root . --suite bot
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

# 中文字体：优先常见 macOS 字体，避免方块字
plt.rcParams["font.sans-serif"] = ["PingFang SC", "Heiti SC", "STHeiti", "Arial Unicode MS", "SimHei", "DejaVu Sans"]
plt.rcParams["axes.unicode_minus"] = False


def load_trigger_jsonl(path: Path) -> pd.DataFrame:
    rows = []
    with path.open(encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            rows.append(json.loads(line))
    return pd.DataFrame(rows)


def load_metrics_csv(path: Path) -> pd.DataFrame:
    df = pd.read_csv(path)
    if "tx_id" in df.columns:
        df = df.dropna(subset=["tx_id"])
    return df


def resolve_trigger_path(root: Path, stem: str) -> Path:
    """
    stem 不含后缀（如 sparrow-amm-trigger）。
    优先 ``<stem>.jsonl``，否则 ``<stem>.json``（兼容旧文件）。
    """
    r = root.resolve()
    p_jsonl = r / f"{stem}.jsonl"
    p_json = r / f"{stem}.json"
    if p_jsonl.is_file():
        return p_jsonl
    if p_json.is_file():
        return p_json
    return p_jsonl


def dataset_presets(root: Path) -> dict[str, tuple[str, Path, list[tuple[str, Path, Path]]]]:
    """suite -> (suptitle, default_png, [(label, csv, jsonl), ...])"""
    return {
        "erc20": (
            "ERC20 跨方案对比（Sparrow / Joyue / 2PC / Chainspace）",
            root / "erc20-metrics-comparison.png",
            [
                ("Sparrow", root / "sparrow-erc20-metrics.csv", resolve_trigger_path(root, "sparrow-erc20-trigger")),
                ("Joyue", root / "joyue-erc20-metrics.csv", resolve_trigger_path(root, "joyue-erc20-trigger")),
                ("2PC", root / "2pc-erc20-metrics.csv", resolve_trigger_path(root, "2pc-erc20-trigger")),
                ("Chainspace", root / "chainspace-erc20-metrics.csv", resolve_trigger_path(root, "chainspace-erc20-trigger")),
            ],
        ),
        "amm": (
            "AMM 跨方案对比（Sparrow / Joyue / 2PC / Chainspace）",
            root / "amm-metrics-comparison.png",
            [
                ("Sparrow", root / "sparrow-amm-metrics.csv", resolve_trigger_path(root, "sparrow-amm-trigger")),
                ("Joyue", root / "joyue-amm-metrics.csv", resolve_trigger_path(root, "joyue-amm-trigger")),
                ("2PC", root / "2pc-amm-metrics.csv", resolve_trigger_path(root, "2pc-amm-trigger")),
                ("Chainspace", root / "chainspace-amm-metrics.csv", resolve_trigger_path(root, "chainspace-amm-trigger")),
            ],
        ),
        "nft": (
            "NFT 跨方案对比（Sparrow / Joyue / 2PC / Chainspace）",
            root / "nft-metrics-comparison.png",
            [
                ("Sparrow", root / "sparrow-nft-metrics.csv", resolve_trigger_path(root, "sparrow-nft-trigger")),
                ("Joyue", root / "joyue-nft-metrics.csv", resolve_trigger_path(root, "joyue-nft-trigger")),
                ("2PC", root / "2pc-nft-metrics.csv", resolve_trigger_path(root, "2pc-nft-trigger")),
                ("Chainspace", root / "chainspace-nft-metrics.csv", resolve_trigger_path(root, "chainspace-nft-trigger")),
            ],
        ),
        "bot": (
            "Bot（MEV Arb）跨方案对比（Sparrow / Joyue / 2PC / Chainspace）",
            root / "bot-metrics-comparison.png",
            [
                ("Sparrow", root / "sparrow-bot-metrics.csv", resolve_trigger_path(root, "sparrow-bot-trigger")),
                ("Joyue", root / "joyue-bot-metrics.csv", resolve_trigger_path(root, "joyue-bot-trigger")),
                ("2PC", root / "2pc-bot-metrics.csv", resolve_trigger_path(root, "2pc-bot-trigger")),
                ("Chainspace", root / "chainspace-bot-metrics.csv", resolve_trigger_path(root, "chainspace-bot-trigger")),
            ],
        ),
    }


def completion_timestamps_ms(metrics: pd.DataFrame) -> pd.Series:
    """完成时间戳（毫秒）：优先 completion_time，缺省或无效则用 created_at。"""
    ca = pd.to_numeric(metrics["created_at"], errors="coerce")
    if "completion_time" not in metrics.columns:
        return ca
    ct = pd.to_numeric(metrics["completion_time"], errors="coerce")
    invalid = ct.isna() | (ct <= 0)
    out = ct.where(~invalid, ca)
    return out


def scheme_mean_span_ms(metrics: pd.DataFrame, trig: pd.DataFrame) -> float | None:
    """
    L = mean(完成时间) − mean(trigger.send_time)，单位毫秒。
    若缺 trigger/metrics、无有效 send 或完成时间、或 L≤0，返回 None。
    """
    if trig.empty or metrics.empty or "send_time" not in trig.columns:
        return None
    st = pd.to_numeric(trig["send_time"], errors="coerce").dropna()
    comp = completion_timestamps_ms(metrics).dropna()
    if st.empty or comp.empty:
        return None
    avg_s = float(st.mean())
    avg_c = float(comp.mean())
    if np.isnan(avg_s) or np.isnan(avg_c):
        return None
    d = avg_c - avg_s
    if d <= 0:
        return None
    return d


def tps_mean_completion_minus_send_window(metrics: pd.DataFrame, trig: pd.DataFrame) -> float:
    """
    TPS = 完成时间落在 [mean(send), mean(completion)] 闭区间内的条数 / (L/1000)，
    L = mean(completion) − mean(trigger.send_time)（毫秒），与本方案内两均值一致。
    """
    span_ms = scheme_mean_span_ms(metrics, trig)
    if span_ms is None or span_ms <= 0:
        return 0.0
    st = pd.to_numeric(trig["send_time"], errors="coerce").dropna()
    comp = completion_timestamps_ms(metrics).dropna()
    if st.empty or comp.empty:
        return 0.0
    lo = float(st.mean())
    hi = float(comp.mean())
    if np.isnan(lo) or np.isnan(hi):
        return 0.0
    n = int(((comp >= lo) & (comp <= hi)).sum())
    return n / (span_ms / 1000.0)


def tps_mean_span_throughput(metrics: pd.DataFrame, trig: pd.DataFrame) -> float:
    """
    TPS（均值窗）：有效完成条数 / ((mean(完成时间) − mean(trigger.send_time))/1000 秒)。
    L 与 scheme_mean_span_ms 一致；分子为 completion_timestamps_ms 去 NaN 后的条数。
    """
    span_ms = scheme_mean_span_ms(metrics, trig)
    if span_ms is None or span_ms <= 0:
        return 0.0
    comp = completion_timestamps_ms(metrics).dropna()
    n = int(len(comp))
    if n == 0:
        return 0.0
    return n / (span_ms / 1000.0)


def classify_outcome(row) -> str:
    """success / failure from metrics row."""
    ok = False
    if "success" in row.index and pd.notna(row["success"]):
        s = str(row["success"]).lower()
        ok = s in ("true", "1", "yes")
    ct = row.get("completion_type", None)
    if pd.notna(ct):
        try:
            ct = int(ct)
            if ct == 1:
                ok = True
            elif ct in (2, 3, 4, 5):
                ok = False
        except (TypeError, ValueError):
            pass
    return "成功" if ok else "失败"


def _norm_shard_series(s: pd.Series) -> pd.Series:
    return s.map(lambda v: "" if pd.isna(v) else str(v).strip())


def _shard_sort_key(s: str) -> tuple:
    try:
        return (0, int(s))
    except ValueError:
        return (1, s)


def joyue_shard_ids_from_trigger(trig: pd.DataFrame) -> list[str]:
    """Joyue 分片列表仅以 trigger 的 shard_id 为准。"""
    if trig.empty or "shard_id" not in trig.columns:
        return []
    raw: set[str] = set()
    for v in trig["shard_id"].tolist():
        if pd.isna(v):
            continue
        t = str(v).strip()
        if t:
            raw.add(t)
    return sorted(raw, key=_shard_sort_key)


def filter_joyue_by_trigger_shard(met: pd.DataFrame, trig: pd.DataFrame, shard: str) -> tuple[pd.DataFrame, pd.DataFrame]:
    """先按 trigger.shard_id 取该分片；metrics 优先用 tx_id 与该片 trigger 对齐。"""
    trig_s = trig[_norm_shard_series(trig["shard_id"]) == shard].copy()
    if "tx_id" in met.columns and "tx_id" in trig_s.columns and not trig_s.empty:
        allow = set(trig_s["tx_id"].dropna().astype(str))
        met_s = met[met["tx_id"].dropna().astype(str).isin(allow)].copy()
    elif "tx_id" in met.columns and "tx_id" in trig.columns:
        met_s = met.iloc[0:0].copy()
    elif "shard_id" in met.columns:
        met_s = met[_norm_shard_series(met["shard_id"]) == shard].copy()
    else:
        met_s = met.iloc[0:0].copy()
    return met_s, trig_s


def iter_scheme_slices(label: str, met: pd.DataFrame, trig: pd.DataFrame):
    """
    Sparrow / 2PC：整表一条序列。
    Joyue：仅当 trigger 含 shard_id 时按分片各一条（标签 Joyue·<shard>）；分片集合只来自 trigger。
    """
    if label != "Joyue":
        yield label, met, trig
        return
    if "shard_id" not in trig.columns:
        yield label, met, trig
        return
    shards = joyue_shard_ids_from_trigger(trig)
    if not shards:
        yield label, met, trig
        return
    for sid in shards:
        ms, ts = filter_joyue_by_trigger_shard(met, trig, sid)
        yield f"Joyue·{sid}", ms, ts


def latency_from_trigger(trig: pd.DataFrame) -> pd.Series:
    """请求确认延迟 (ms) = receipt_block_time − send_time；有 tx_id 时按 tx_id 去重保留首条。"""
    if trig.empty:
        return pd.Series(dtype=float)
    if "receipt_block_time" not in trig.columns or "send_time" not in trig.columns:
        return pd.Series(dtype=float)
    t = trig
    if "tx_id" in t.columns:
        t = t.drop_duplicates(subset=["tx_id"], keep="first")
    rb = pd.to_numeric(t["receipt_block_time"], errors="coerce")
    st = pd.to_numeric(t["send_time"], errors="coerce")
    lat = (rb - st).astype(float)
    return lat.replace([np.inf, -np.inf], np.nan).dropna()


def main() -> None:
    ap = argparse.ArgumentParser(description="Plot ERC20, AMM, NFT, or Bot Sparrow/Joyue/2PC/Chainspace comparison.")
    ap.add_argument("--suite", choices=("erc20", "amm", "nft", "bot"), default="erc20", help="which file set to load")
    ap.add_argument("--out", type=Path, default=None, help="output PNG (default: suite-specific under --root)")
    ap.add_argument("--root", type=Path, default=Path("."))
    args = ap.parse_args()
    root = args.root.resolve()
    presets = dataset_presets(root)
    suptitle, default_out, sets = presets[args.suite]
    out_path = args.out.resolve() if args.out is not None else default_out

    names: list[str] = []
    succ: list[int] = []
    fail: list[int] = []
    latencies: list[np.ndarray] = []
    # TPS 子图专用：Joyue 合并为单柱，与 names 长度可不同
    tps_bar_labels: list[str] = []
    tps_bar_vals: list[float] = []

    for label, csv_p, jsonl_p in sets:
        if not csv_p.is_file() or not jsonl_p.is_file():
            print(f"skip {label}: missing {csv_p} or {jsonl_p}")
            continue
        met = load_metrics_csv(csv_p)
        trig = load_trigger_jsonl(jsonl_p)
        if trig.empty:
            print(
                f"warning: {label} trigger 无有效 JSONL 行（文件为空或仅空白）"
                f" → TPS/延迟 ECDF 无该方案曲线: {jsonl_p}"
            )
        if met.empty:
            print(
                f"warning: {label} metrics 无数据行（仅表头或空表）"
                f" → 堆叠柱成功/失败均为 0: {csv_p}"
            )
        joyue_tps_merged = tps_mean_completion_minus_send_window(met, trig) if label == "Joyue" else None
        joyue_tps_bar_done = False
        for slice_label, met_s, trig_s in iter_scheme_slices(label, met, trig):
            names.append(slice_label)
            outcomes = met_s.apply(classify_outcome, axis=1)
            succ.append(int((outcomes == "成功").sum()))
            fail.append(int((outcomes == "失败").sum()))
            if label == "Joyue":
                if not joyue_tps_bar_done:
                    tps_bar_labels.append("Joyue")
                    tps_bar_vals.append(float(joyue_tps_merged))
                    joyue_tps_bar_done = True
            else:
                tps_bar_labels.append(slice_label)
                tps_bar_vals.append(tps_mean_completion_minus_send_window(met_s, trig_s))
            lat = latency_from_trigger(trig_s)
            latencies.append(lat.to_numpy() if len(lat) else np.array([]))

    if not names:
        raise SystemExit("no datasets loaded")

    fig_w = min(22.0, max(10.0, 5.5 + 1.35 * len(names)))
    fig, axes = plt.subplots(2, 2, figsize=(fig_w, 9))
    fig.suptitle(suptitle, fontsize=14, fontweight="bold")

    # 1) 堆叠柱状图：成功 / 失败
    ax1 = axes[0, 0]
    x = np.arange(len(names))
    w = 0.55
    b1 = ax1.bar(x, succ, w, label="成功", color="#2ca02c")
    b2 = ax1.bar(x, fail, w, bottom=succ, label="失败", color="#d62728")
    ax1.set_xticks(x)
    ax1.set_xticklabels(names, rotation=22, ha="right")
    ax1.set_ylabel("请求数")
    ax1.set_title("完成结果（堆叠）")
    ax1.legend()
    ax1.grid(axis="y", alpha=0.3)
    for i, (s, f) in enumerate(zip(succ, fail)):
        total = s + f
        ax1.text(i, total + max(1, total * 0.02), f"共{total}", ha="center", va="bottom", fontsize=9)

    # 2) TPS：各方案 L=mean(完成)−mean(send)；计数 ∈ [mean(send), mean(完成)] / (L/1000s)（Joyue 整表）
    ax2 = axes[0, 1]
    cmap = plt.get_cmap("tab10")
    nq = len(tps_bar_labels)
    bar_colors = [cmap(i % 10) for i in range(nq)]
    bars = ax2.bar(tps_bar_labels, tps_bar_vals, color=bar_colors)
    ax2.set_ylabel("TPS（完成）")
    ax2.set_title(
        "完成 TPS（Joyue 全 shard 合并）\n"
        "窗内完成数 / (mean(完成时间) − mean(send_time))\n"
        "区间：[mean(send), mean(完成)]；send：trigger；完成：completion_time，缺省 created_at"
    )
    ax2.grid(axis="y", alpha=0.3)
    for b, q in zip(bars, tps_bar_vals):
        ax2.text(b.get_x() + b.get_width() / 2, b.get_height(), f"{q:.1f}", ha="center", va="bottom", fontsize=9)
    ax2.tick_params(axis="x", rotation=22)

    # 3) 延迟 ECDF：各序列累积分布同图对比
    ax3 = axes[1, 0]
    line_cmap = plt.get_cmap("tab10")
    any_lat = False
    for i, (label, arr) in enumerate(zip(names, latencies)):
        if len(arr) == 0:
            continue
        any_lat = True
        x = np.sort(arr.astype(float))
        y = np.arange(1, len(x) + 1, dtype=float) / len(x)
        c = line_cmap(i % 10)
        ax3.step(x, y, where="post", label=label, color=c, linewidth=2)
    if any_lat:
        ax3.set_xlabel("延迟 (ms)")
        ax3.set_ylabel("累积比例 F(x) = P(延迟 ≤ x)")
        ax3.set_title("请求确认延迟 ECDF\n(trigger.receipt_block_time − trigger.send_time)")
        ax3.set_ylim(0, 1.02)
        ax3.legend(loc="lower right")
        ax3.grid(True, alpha=0.3)
    else:
        ax3.text(0.5, 0.5, "无可用延迟样本（检查 trigger 含 receipt_block_time / send_time）", ha="center", va="center", transform=ax3.transAxes)

    # 4) 分位数表（文本）
    ax4 = axes[1, 1]
    ax4.axis("off")
    lines = ["分位数 (ms)", "—" * 28]
    for i, label in enumerate(names):
        arr = latencies[i]
        if len(arr) == 0:
            lines.append(f"{label}: 无数据")
            continue
        p50, p90, p95, p99 = np.percentile(arr, [50, 90, 95, 99])
        lines.append(f"{label}:")
        lines.append(f"  n={len(arr)}  p50={p50:.0f}  p90={p90:.0f}  p95={p95:.0f}  p99={p99:.0f}")
    ax4.text(0.02, 0.98, "\n".join(lines), transform=ax4.transAxes, va="top", fontsize=10)
    ax4.set_title("延迟摘要")

    fig.tight_layout()
    fig.savefig(out_path, dpi=150, bbox_inches="tight")
    print(f"written {out_path}")


if __name__ == "__main__":
    main()
