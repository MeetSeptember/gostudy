import pandas as pd
import json
import matplotlib.pyplot as plt
import numpy as np

def process_robust_metrics(trig_file, met_file, type_name, shard_count):
    # Load Trigger
    trig_data = []
    with open(trig_file, 'r') as f:
        for line in f:
            trig_data.append(json.loads(line))
    df_trig = pd.DataFrame(trig_data)

    # Load Metrics
    df_met = pd.read_csv(met_file)

    # Merge
    df_merged = pd.merge(df_trig[['tx_id', 'send_time']].dropna(subset=['tx_id']),
                         df_met[['tx_id', 'completion_time']],
                         on='tx_id')

    # Calculate Latency
    df_merged['latency_s'] = (df_merged['completion_time'] - df_merged['send_time']) / 1000.0
    # Clean noise (clock sync)
    df_merged = df_merged[df_merged['latency_s'] >= 0]

    if df_merged.empty:
        return None

    # Calculate Robust TPS (95% of transactions)
    # Sort by completion time to find the window for the first 95% of completions
    df_sorted = df_merged.sort_values('completion_time')
    n_95 = int(len(df_sorted) * 0.95)
    if n_95 == 0: n_95 = 1

    df_95 = df_sorted.iloc[:n_95]
    t_start = df_95['send_time'].min()
    t_end = df_95['completion_time'].max()

    duration_95 = (t_end - t_start) / 1000.0
    # Effective TPS is count of these transactions / duration
    robust_tps = n_95 / duration_95 if duration_95 > 0 else 0

    return {
        'Type': type_name,
        'Shards': shard_count,
        'Robust_TPS': robust_tps,
        'Avg_Latency': df_merged['latency_s'].mean(),
        'P95_Latency': df_merged['latency_s'].quantile(0.95),
        'P99_Latency': df_merged['latency_s'].quantile(0.99)
    }

configs = {
    'BOT': {
        2: ('joyue-bot-trigger.jsonl', 'joyue-bot-metrics.csv'),
        4: ('joyue-bot-trigger-4.jsonl', 'joyue-bot-metrics-4.csv'),
        8: ('joyue-bot-trigger-8.jsonl', 'joyue-bot-metrics-8.csv')
    },
    'NFT': {
        2: ('joyue-nft-trigger.jsonl', 'joyue-nft-metrics.csv'),
        4: ('joyue-nft-trigger-4.jsonl', 'joyue-nft-metrics-4.csv'),
        8: ('joyue-nft-trigger-8.jsonl', 'joyue-nft-metrics-8.csv')
    },
    'ERC20': {
        2: ('joyue-erc20-trigger.jsonl', 'joyue-erc20-metrics.csv'),
        4: ('joyue-erc20-trigger-4.jsonl', 'joyue-erc20-metrics-4.csv'),
        8: ('joyue-erc20-trigger-8.jsonl', 'joyue-erc20-metrics-8.csv')
    },
    'AMM': {
        2: ('joyue-amm-trigger.jsonl', 'joyue-amm-metrics.csv'),
        4: ('joyue-amm-trigger-4.jsonl', 'joyue-amm-metrics-4.csv'),
        8: ('joyue-amm-trigger-8.jsonl', 'joyue-amm-metrics-8.csv')
    }
}

final_results = []
for t_name, shard_map in configs.items():
    for s_count, (t_file, m_file) in shard_map.items():
        res = process_robust_metrics(t_file, m_file, t_name, s_count)
        if res:
            final_results.append(res)

df_robust = pd.DataFrame(final_results)

# Plotting
fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(18, 7))

contract_types = df_robust['Type'].unique()
colors = plt.cm.tab10(np.linspace(0, 1, len(contract_types)))

for i, c_type in enumerate(contract_types):
    subset = df_robust[df_robust['Type'] == c_type].sort_values('Shards')

    # Plot Robust TPS
    ax1.plot(subset['Shards'], subset['Robust_TPS'], marker='o', label=c_type, color=colors[i], linewidth=2.5, markersize=8)

    # Plot Avg Latency
    ax2.plot(subset['Shards'], subset['Avg_Latency'], marker='s', label=c_type, color=colors[i], linewidth=2.5, markersize=8)

ax1.set_title('Robust Throughput (TPS) vs Shard Count\n(Based on 95% of Transactions)', fontsize=14)
ax1.set_xlabel('Number of Shards', fontsize=12)
ax1.set_ylabel('Effective TPS', fontsize=12)
ax1.set_xticks([2, 4, 8])
ax1.legend(title='Contract Type')
ax1.grid(True, linestyle='--', alpha=0.7)

ax2.set_title('Confirmation Latency vs Shard Count', fontsize=14)
ax2.set_xlabel('Number of Shards', fontsize=12)
ax2.set_ylabel('Latency (seconds)', fontsize=12)
ax2.set_xticks([2, 4, 8])
ax2.legend(title='Contract Type')
ax2.grid(True, linestyle='--', alpha=0.7)

plt.tight_layout()
plt.savefig('robust_performance_comparison.png')

print(df_robust[['Type', 'Shards', 'Robust_TPS', 'Avg_Latency']])