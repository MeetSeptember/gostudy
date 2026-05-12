#!/usr/bin/env python3
"""
Version 2：按「类型」分组柱状图（x = ERC20 / AMM / NFT / Bot），每组 4 根柱对应
Sparrow / Joyue / 2PC / Chainspace（样式参考分组柱：颜色 + hatch + 黑边；Chainspace 柱为横线纹理）。

- TPS（**一张**分组柱）：有效完成条数 / W，W(秒) = (max(完成时间) − min(trigger.send_time)) / 1000；
  完成时间用 ``completion_timestamps_ms``（与 plot_erc20_metrics_comparison 一致）。
- 任务成功率：折线图，x=合约类型，每条线=一种实现；y=成功条数/总条数。
- 平均确认延迟：分组柱（与 TPS 相同 x/分组），每根柱为按 tx_id 对齐后 (completion−trigger.send) 的样本均值（毫秒）。
- 锁 abort 率（临时）：折线图，x=合约类型，每条线=一种实现；y=失败条数/总条数（失败暂视为锁 abort）。

示例：
  python3 tools/plot_metrics_comparison_v2.py --root .
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

# 与参考图：四合约颜色 + 斜线/交叉/点状 hatch + 黑边
SUITES_ORDER = ("erc20", "amm", "nft", "bot")
SUITE_X_LABELS = ("ERC20", "AMM", "NFT", "Bot")

# (填充色, hatch) — 与参考分组柱：绿/、黄\、蓝 x、橙横线 + 黑边在 plot_grouped_metric 中统一加
STYLE_BY_SCHEME: dict[str, tuple[str, str]] = {
    "Sparrow": ("#b8e0b8", "/"),
    "Joyue": ("#fff9c4", r"\\"),
    "2PC": ("#c5d8f0", "x"),
    "Chainspace": ("#ffd4b8", "-"),
}

# 折线图（成功率 / abort）：颜色 + marker（黄▼、紫▲、横线、绿圆），线宽、标记大小
SCHEME_LINE_STYLE: dict[str, tuple[str, str, float, float]] = {
    "Joyue": ("#e6c200", "v", 2.3, 9.0),  # 黄，下三角
    "2PC": ("#8e44ad", "^", 2.3, 9.0),  # 紫，上三角
    "Chainspace": ("#2980b9", "_", 2.3, 14.0),  # 蓝，横线标记
    "Sparrow": ("#27ae60", "o", 2.3, 10.0),  # 绿，圆略大
}


def mean_send_to_completion_ms(metrics: pd.DataFrame, trig: pd.DataFrame) -> float:
    """
    平均确认延迟（毫秒）：metrics 完成时间 − trigger.send_time，按 tx_id 对齐；
    trigger 同 tx_id 多条时保留首条。若无 tx_id 可对齐，退化为 mean(完成)−mean(send)。
    """
    if metrics.empty or trig.empty or "send_time" not in trig.columns:
        return 0.0
    comp_series = m1.completion_timestamps_ms(metrics)
    if (
        "tx_id" in metrics.columns
        and "tx_id" in trig.columns
        and not comp_series.isna().all()
    ):
        right = trig[["tx_id", "send_time"]].drop_duplicates(subset=["tx_id"], keep="first")
        right = right.assign(k=right["tx_id"].astype(str)).rename(columns={"send_time": "tr_send"})[
            ["k", "tr_send"]
        ]
        left = metrics.assign(_comp=comp_series, k=metrics["tx_id"].astype(str))[["k", "_comp"]]
        j = left.merge(right, on="k", how="inner")
        if not j.empty:
            c = pd.to_numeric(j["_comp"], errors="coerce")
            s = pd.to_numeric(j["tr_send"], errors="coerce")
            dur = (c - s).replace([np.inf, -np.inf], np.nan).dropna()
            dur = dur[dur >= 0]
            if len(dur) > 0:
                return float(dur.mean())
    st = pd.to_numeric(trig["send_time"], errors="coerce").dropna()
    comp = comp_series.dropna()
    if st.empty or comp.empty:
        return 0.0
    return float(max(0.0, comp.mean() - st.mean()))


def build_matrices(root: Path) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray, list[str]]:
    """shape (4 suites, n_schemes): TPS（min(send)～max(完成) 窗）、确认延迟、成功率、abort。"""
    root = root.resolve()
    presets = m1.dataset_presets(root)
    ref_sets = presets["erc20"][2]
    scheme_labels = [label for label, _c, _j in ref_sets]
    n_su = len(SUITES_ORDER)
    n_sc = len(scheme_labels)
    tps_m = np.zeros((n_su, n_sc))
    exec_m = np.zeros((n_su, n_sc))
    success_m = np.zeros((n_su, n_sc))
    abort_m = np.zeros((n_su, n_sc))

    for i, suite in enumerate(SUITES_ORDER):
        _title, _png, sets = presets[suite]
        if len(sets) != n_sc:
            raise SystemExit(f"preset {suite}: scheme count != erc20 ({len(sets)} vs {n_sc})")
        for j, (scheme_label, csv_p, jsonl_p) in enumerate(sets):
            if scheme_label != scheme_labels[j]:
                raise SystemExit(f"preset {suite} col {j}: expected {scheme_labels[j]}, got {scheme_label}")
            if not csv_p.is_file() or not jsonl_p.is_file():
                continue
            met = m1.load_metrics_csv(csv_p)
            trig = m1.load_trigger_jsonl(jsonl_p)
            tps_m[i, j] = m1.tps_min_send_max_completion_window(met, trig)
            exec_m[i, j] = mean_send_to_completion_ms(met, trig)
            if met.empty:
                continue
            outcomes = met.apply(m1.classify_outcome, axis=1)
            n = len(outcomes)
            if n == 0:
                continue
            fails = int((outcomes == "失败").sum())
            oks = int((outcomes == "成功").sum())
            abort_m[i, j] = fails / float(n)
            success_m[i, j] = oks / float(n)
    return tps_m, exec_m, success_m, abort_m, scheme_labels


def plot_grouped_metric(
    ax,
    matrix: np.ndarray,
    schemes: list[str],
    ylabel: str,
    title: str,
    value_fmt: str,
) -> None:
    n_types, n_schemes = matrix.shape
    x = np.arange(n_types)
    width = 0.2
    offsets = (np.arange(n_schemes) - (n_schemes - 1) / 2.0) * width

    for j in range(n_schemes):
        vals = matrix[:, j]
        color, hatch = STYLE_BY_SCHEME.get(schemes[j], ("#dddddd", ""))
        bars = ax.bar(
            x + offsets[j],
            vals,
            width,
            label=schemes[j],
            color=color,
            hatch=hatch,
            edgecolor="black",
            linewidth=0.8,
        )
        for b in bars:
            h = b.get_height()
            if h <= 0:
                continue
            ax.text(
                b.get_x() + b.get_width() / 2.0,
                h,
                value_fmt.format(h),
                ha="center",
                va="bottom",
                fontsize=7,
            )

    ax.set_xticks(x)
    ax.set_xticklabels(SUITE_X_LABELS)
    ax.set_ylabel(ylabel)
    ax.set_title(title)
    ax.legend(loc="upper right", fontsize=8)
    ax.grid(True, which="major", linestyle="--", alpha=0.45)
    ax.set_axisbelow(True)


def plot_implementation_lines(
    ax,
    matrix: np.ndarray,
    schemes: list[str],
    ylabel: str,
    title: str,
    *,
    y_top: float | None = None,
) -> None:
    """x=合约类型，每条线=一种实现；样式与 abort 折线一致。"""
    n_types, n_schemes = matrix.shape
    x = np.arange(n_types)
    ax.set_facecolor("#f0f2ec")
    for j in range(n_schemes):
        name = schemes[j]
        y = matrix[:, j]
        st = SCHEME_LINE_STYLE.get(name, ("#555555", "o", 2.0, 8.0))
        color, marker, lw, ms = st[0], st[1], st[2], st[3]
        ax.plot(
            x,
            y,
            color=color,
            marker=marker,
            markersize=ms,
            linewidth=lw,
            label=name,
            markerfacecolor=color,
            markeredgecolor="black",
            markeredgewidth=0.7,
            clip_on=False,
        )
    ax.set_xticks(x)
    ax.set_xticklabels(SUITE_X_LABELS)
    ax.set_ylabel(ylabel)
    ax.set_title(title)
    if y_top is not None:
        ax.set_ylim(0.0, y_top)
    else:
        ax.set_ylim(bottom=0.0)
    ax.legend(loc="best", fontsize=9)
    ax.grid(True, which="major", linestyle="--", alpha=0.5, color="gray")
    ax.set_axisbelow(True)


def main() -> None:
    ap = argparse.ArgumentParser(
        description="Metrics v2: one TPS bar chart (min(send)–max(completion) window) + latency + success/abort."
    )
    ap.add_argument("--root", type=Path, default=Path("."))
    ap.add_argument("--out", type=Path, default=None, help="output PNG (default: <root>/metrics-comparison-v2.png)")
    args = ap.parse_args()
    root = args.root.resolve()
    out = args.out.resolve() if args.out is not None else root / "metrics-comparison-v2.png"

    tps_m, exec_m, success_m, abort_m, schemes = build_matrices(root)

    fig, axes = plt.subplots(4, 1, figsize=(10, 13), constrained_layout=True)
    fig.suptitle("跨类型对比（v2）", fontsize=14, fontweight="bold")

    plot_grouped_metric(
        axes[0],
        tps_m,
        schemes,
        "TPS",
        "完成 TPS\nW=(max(完成时间)−min(send_time))/1s；分子=有效完成条数（completion_timestamps_ms）",
        "{:.1f}",
    )
    plot_grouped_metric(
        axes[1],
        exec_m,
        schemes,
        "平均确认延迟 (ms)",
        "平均确认延迟（分组柱）\ntrigger.send_time → metrics 完成时间；按 tx_id 对齐取样本均值",
        "{:.0f}",
    )
    plot_implementation_lines(
        axes[2],
        success_m,
        schemes,
        "任务成功率",
        "任务成功率（折线：成功条数 / 总条数）",
        y_top=1.02,
    )
    plot_implementation_lines(
        axes[3],
        abort_m,
        schemes,
        "锁 abort 率（临时）",
        "锁 abort 率（折线：失败样本暂视为锁 abort）",
        y_top=1.02,
    )

    fig.text(
        0.5,
        0.01,
        "说明：TPS 分母 W 为观测窗（秒）= (metrics 上 max(完成时间) − trigger 上 min(send_time))/1000，"
        "完成时间规则同 plot_erc20_metrics_comparison.completion_timestamps_ms；分子为该表上有效完成条数。"
        "确认延迟按 tx_id 对齐；成功率=成功/总数；abort率=失败/总数（失败暂视为锁 abort）。",
        ha="center",
        fontsize=8,
        style="italic",
    )

    fig.savefig(out, dpi=150, bbox_inches="tight")
    print(f"written {out}")


if __name__ == "__main__":
    main()
