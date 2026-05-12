#!/usr/bin/env python3
"""
Joyue：按「确认延迟分桶」统计 TPS，并按合约类型 × 分片数作图。

- 数据约定（与其它绘图脚本一致）：
  - **2 分片**：``joyue-{suite}-metrics.csv`` + ``joyue-{suite}-trigger``（优先 ``*.jsonl``）。
  - **N≠2 分片**：``joyue-{suite}-metrics-N.csv`` + ``joyue-{suite}-trigger-N``。

- **仅用 metrics 的 ``completion_time``**（无有效 completion_time 的行不参与统计）。
- **send_time** 来自 trigger，与 metrics 按 ``tx_id`` 对齐（trigger 同 tx 多条取首条）。
- **确认延迟**（毫秒）：``completion_time - send_time``（仅保留 ≥0 且有效的样本）。
- **按秒分桶**：默认 [0,1)、[1,2)、[2,3)…（``--bin-sec`` / ``--max-bins`` 可调）。
- **每桶 TPS**：桶内条数 ``N_bin`` / 观测窗口 ``W``（秒），其中
  ``W = (max(completion_time) − min(send_time)) / 1000``，在 **当前 dataset（某 suite×某分片）的对齐样本上** 计算。

- **图**：每个延迟桶 **一个子图**。子图内 **x = 合约类型**（ERC20/AMM/NFT/Bot），**y = 该桶 TPS**；
  **每条折线 = 一种分片数量**（``--shards``）。

示例::

  python3 tools/plot_joyue_shard_scaling.py --root .
  python3 tools/plot_joyue_shard_scaling.py --root . --shards 2,4,8,16 --bin-sec 1 --max-bins 8
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

_TOOLS = Path(__file__).resolve().parent
if str(_TOOLS) not in sys.path:
    sys.path.insert(0, str(_TOOLS))

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np
import pandas as pd

import plot_erc20_metrics_comparison as m1

plt.rcParams["font.sans-serif"] = ["PingFang SC", "Heiti SC", "STHeiti", "Arial Unicode MS", "SimHei", "DejaVu Sans"]
plt.rcParams["axes.unicode_minus"] = False

SUITES = ("erc20", "amm", "nft", "bot")
SUITE_DISPLAY = ("ERC20", "AMM", "NFT", "Bot")


def parse_shards(s: str) -> list[int]:
    """逗号分隔的正整数，去重后升序。"""
    out: list[int] = []
    for part in s.split(","):
        part = part.strip()
        if not part:
            continue
        try:
            n = int(part, 10)
        except ValueError as e:
            raise argparse.ArgumentTypeError(f"invalid shard token {part!r}") from e
        if n <= 0:
            raise argparse.ArgumentTypeError(f"shard count must be positive, got {n}")
        out.append(n)
    if not out:
        raise argparse.ArgumentTypeError("empty --shards")
    return sorted(set(out))


def joyue_paths(root: Path, suite: str, n_shards: int) -> tuple[Path, Path]:
    root = root.resolve()
    if n_shards == 2:
        csv_p = root / f"joyue-{suite}-metrics.csv"
        stem = f"joyue-{suite}-trigger"
    else:
        csv_p = root / f"joyue-{suite}-metrics-{n_shards}.csv"
        stem = f"joyue-{suite}-trigger-{n_shards}"
    trig_p = m1.resolve_trigger_path(root, stem)
    return csv_p, trig_p


def aligned_latency_ms_completion_only(metrics: pd.DataFrame, trig: pd.DataFrame) -> pd.Series:
    """
    仅用 completion_time；返回对齐后的确认延迟序列（毫秒），与 trig.send_time 按 tx_id 对齐。
    """
    if trig.empty or metrics.empty or "send_time" not in trig.columns:
        return pd.Series(dtype=float)
    if "completion_time" not in metrics.columns:
        return pd.Series(dtype=float)
    if "tx_id" not in metrics.columns or "tx_id" not in trig.columns:
        return pd.Series(dtype=float)

    met = metrics.dropna(subset=["tx_id", "completion_time"]).copy()
    met["tx_id"] = met["tx_id"].astype(str)
    tr = (
        trig.dropna(subset=["tx_id", "send_time"])
        .drop_duplicates(subset=["tx_id"], keep="first")[["tx_id", "send_time"]]
        .rename(columns={"send_time": "tr_send_time"})
    )
    tr["tx_id"] = tr["tx_id"].astype(str)
    j = met.merge(tr, on="tx_id", how="inner")
    if j.empty:
        return pd.Series(dtype=float)

    ct = pd.to_numeric(j["completion_time"], errors="coerce")
    st = pd.to_numeric(j["tr_send_time"], errors="coerce")
    lat = ct - st
    ok = ct.notna() & st.notna() & lat.notna() & (lat >= 0)
    return lat[ok].astype(float)


def window_sec_from_joined(metrics: pd.DataFrame, trig: pd.DataFrame) -> float | None:
    """W = (max(completion_time) - min(send_time)) / 1000，仅需 completion_time 有效的对齐行。"""
    if trig.empty or metrics.empty or "completion_time" not in metrics.columns:
        return None
    if "tx_id" not in metrics.columns or "tx_id" not in trig.columns:
        return None
    met = metrics.dropna(subset=["tx_id", "completion_time"]).copy()
    met["tx_id"] = met["tx_id"].astype(str)
    tr = (
        trig.dropna(subset=["tx_id", "send_time"])
        .drop_duplicates(subset=["tx_id"], keep="first")[["tx_id", "send_time"]]
        .rename(columns={"send_time": "tr_send_time"})
    )
    tr["tx_id"] = tr["tx_id"].astype(str)
    j = met.merge(tr, on="tx_id", how="inner")
    if j.empty:
        return None
    ct = pd.to_numeric(j["completion_time"], errors="coerce")
    st = pd.to_numeric(j["tr_send_time"], errors="coerce")
    ok = ct.notna() & st.notna() & (ct - st >= 0)
    if not ok.any():
        return None
    w_ms = float(ct[ok].max() - st[ok].min())
    if not np.isfinite(w_ms) or w_ms <= 0:
        return None
    return w_ms / 1000.0


def tps_by_latency_bins(
    lat_ms: pd.Series,
    window_sec: float,
    bin_sec: float,
    max_bins: int,
) -> list[tuple[str, float]]:
    """
    返回 [(桶标签, TPS), ...]，长度最多 max_bins。
    桶 [k*bin_sec, (k+1)*bin_sec) 秒；TPS = 桶内条数 / window_sec。
    """
    if lat_ms.empty or window_sec <= 0 or not np.isfinite(window_sec):
        return []
    lat_sec = lat_ms.astype(float) / 1000.0
    out: list[tuple[str, float]] = []
    for k in range(max_bins):
        lo, hi = k * bin_sec, (k + 1) * bin_sec
        n = int(((lat_sec >= lo) & (lat_sec < hi)).sum())
        label = f"[{lo:.0f},{hi:.0f})s"
        out.append((label, n / window_sec))
    return out


def load_dataset_tps_bins(
    csv_p: Path,
    trig_p: Path,
    bin_sec: float,
    max_bins: int,
) -> tuple[list[tuple[str, float]] | None, str | None]:
    """返回 (bins list or None, skip reason or None)。"""
    if not csv_p.is_file() or not trig_p.is_file():
        return None, "missing file"
    met = m1.load_metrics_csv(csv_p)
    trig = m1.load_trigger_jsonl(trig_p)
    W = window_sec_from_joined(met, trig)
    if W is None:
        return None, "no aligned completion_time window"
    lat = aligned_latency_ms_completion_only(met, trig)
    if lat.empty:
        return None, "no aligned latency samples"
    bins = tps_by_latency_bins(lat, W, bin_sec, max_bins)
    return bins, None


def main() -> None:
    ap = argparse.ArgumentParser(
        description="Joyue：按确认延迟分桶 TPS；x=合约类型，y=TPS；每条线=一分片配置。"
    )
    ap.add_argument("--root", type=Path, default=Path("."))
    ap.add_argument(
        "--shards",
        type=parse_shards,
        default=parse_shards("2,4,8"),
        help="分片数量列表（默认 2,4,8）",
    )
    ap.add_argument("--bin-sec", type=float, default=1.0, help="延迟桶宽度（秒），默认 1")
    ap.add_argument("--max-bins", type=int, default=8, help="最多画几个延迟桶（默认 8，即 0–8s）")
    ap.add_argument("--out", type=Path, default=None, help="输出 PNG（默认 <root>/joyue-shard-scaling.png）")
    args = ap.parse_args()

    if args.bin_sec <= 0:
        raise SystemExit("--bin-sec must be positive")
    if args.max_bins < 1:
        raise SystemExit("--max-bins must be >= 1")

    root = args.root.resolve()
    shard_list = args.shards
    out = args.out.resolve() if args.out is not None else root / "joyue-shard-scaling.png"

    # 缓存： (suite, shard_n) -> list of (label, tps)
    cache: dict[tuple[str, int], list[tuple[str, float]]] = {}
    for suite in SUITES:
        for n in shard_list:
            csv_p, trig_p = joyue_paths(root, suite, n)
            bins, reason = load_dataset_tps_bins(csv_p, trig_p, args.bin_sec, args.max_bins)
            if bins is None:
                print(f"skip {suite} @ {n} shards ({reason}): csv={csv_p.name}")
                continue
            cache[(suite, n)] = bins

    if not cache:
        raise SystemExit("no datasets loaded; nothing to plot")

    # 桶标签以首个可用序列为准
    sample_bins = next(iter(cache.values()))
    bin_labels = [b[0] for b in sample_bins]
    n_bins = len(bin_labels)

    cmap = plt.cm.tab10(np.linspace(0, 1, max(10, len(shard_list))))
    x_idx = np.arange(len(SUITES))

    fig, axes = plt.subplots(n_bins, 1, figsize=(9, 2.8 + 2.2 * n_bins), constrained_layout=True)
    if n_bins == 1:
        axes = [axes]

    for bi, blab in enumerate(bin_labels):
        ax = axes[bi]
        ax.set_facecolor("#f0f2ec")
        for si, n_sh in enumerate(shard_list):
            color = cmap[si % len(cmap)]
            ys: list[float] = []
            for suite in SUITES:
                key = (suite, n_sh)
                if key not in cache:
                    ys.append(float("nan"))
                    continue
                row = cache[key]
                # 同索引对应同桶
                if bi >= len(row):
                    ys.append(float("nan"))
                else:
                    ys.append(row[bi][1])
            ax.plot(
                x_idx,
                ys,
                color=color,
                marker="o",
                markersize=7,
                linewidth=2.0,
                label=f"{n_sh} 分片",
                markeredgecolor="black",
                markeredgewidth=0.6,
                clip_on=False,
            )
        ax.set_xticks(x_idx)
        ax.set_xticklabels(list(SUITE_DISPLAY))
        ax.set_ylabel("TPS")
        ax.set_title(f"确认延迟 {blab}（TPS = 桶内条数 / W；W=(max(completion)−min(send))/1s）", fontsize=10)
        ax.set_ylim(bottom=0.0)
        ax.grid(True, which="major", linestyle="--", alpha=0.45, color="gray")
        ax.legend(loc="best", fontsize=8, ncol=min(4, len(shard_list)))
        ax.set_axisbelow(True)

    fig.suptitle(
        "Joyue：按 completion_time 对齐分桶 TPS（仅用 completion_time；tx_id 对齐 trigger）",
        fontsize=12,
        fontweight="bold",
    )
    fig.savefig(out, dpi=150, bbox_inches="tight")
    print(f"written {out}")


if __name__ == "__main__":
    main()
