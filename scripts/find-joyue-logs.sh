#!/bin/bash
# 查找 JOYUE 相关日志的脚本

LOG_DIR="${1:-tmp_log}"

echo "=== 查找 JOYUE 日志 ==="
echo "搜索目录: $LOG_DIR"
echo ""

# 检查目录是否存在
if [ ! -d "$LOG_DIR" ]; then
    echo "错误: 目录不存在: $LOG_DIR"
    exit 1
fi

# 1. 查找最新的日志目录
echo "1. 查找最新的日志目录："
LATEST_LOG_DIR=$(ls -td "$LOG_DIR"/log-* 2>/dev/null | head -1)
if [ -z "$LATEST_LOG_DIR" ]; then
    echo "   未找到日志目录"
    exit 1
fi
echo "   最新日志目录: $LATEST_LOG_DIR"
echo ""

# 2. 列出所有日志文件
echo "2. 日志文件列表："
find "$LATEST_LOG_DIR" -name "*.log" -type f 2>/dev/null | while read -r logfile; do
    filename=$(basename "$logfile")
    # 尝试获取文件大小（兼容 macOS 和 Linux）
    if command -v stat >/dev/null 2>&1; then
        size=$(stat -f%z "$logfile" 2>/dev/null || stat -c%s "$logfile" 2>/dev/null)
        if [ -n "$size" ]; then
            size_mb=$(awk "BEGIN {printf \"%.2f\", $size/1024/1024}")
            echo "   $filename ($size_mb MB)"
        else
            echo "   $filename"
        fi
    else
        echo "   $filename"
    fi
done
echo ""

# 3. 在所有日志文件中查找 JOYUE 运行日志（排除配置中的 JOYUE 字段）
echo "3. 查找 JOYUE 运行日志（排除配置）："
JOYUE_COUNT=0
find "$LATEST_LOG_DIR" -name "*.log" -type f 2>/dev/null | while read -r logfile; do
    # 搜索 JOYUE，但排除配置中的 "Joyue":{ 字段
    # 查找包含 [JOYUE] 或 message 中包含 JOYUE 的日志
    matches=$(grep -i "joyue" "$logfile" 2>/dev/null | grep -v '"Joyue":' | grep -v '"joyue":' | grep -E '\[JOYUE\]|"message".*[Jj][Oo][Yy][Uu][Ee]' | wc -l | awk '{print $1}')
    if [ -n "$matches" ] && [ "$matches" -gt 0 ] 2>/dev/null; then
        echo ""
        echo "   === $(basename "$logfile") (找到 $matches 条运行日志) ==="
        # 显示匹配的日志
        grep -i "joyue" "$logfile" 2>/dev/null | grep -v '"Joyue":' | grep -v '"joyue":' | grep -E '\[JOYUE\]|"message".*[Jj][Oo][Yy][Uu][Ee]' | head -20 | while IFS= read -r line; do
            # 如果是 JSON 格式，尝试美化输出
            if echo "$line" | grep -q "^{"; then
                # JSON 格式：尝试提取关键字段
                if command -v python3 >/dev/null 2>&1; then
                    formatted=$(echo "$line" | python3 -m json.tool 2>/dev/null)
                    if [ $? -eq 0 ]; then
                        # 提取关键信息
                        echo "$formatted" | grep -E "(level|time|message|shard|codeLen|codeHash|txHash|master|error)" | head -8
                    else
                        echo "   $line"
                    fi
                else
                    # 简单提取 message 字段
                    echo "$line" | grep -o '"message":"[^"]*"' | head -1 || echo "   $line"
                fi
            else
                # 普通文本格式
                echo "   $line"
            fi
        done
        JOYUE_COUNT=$((JOYUE_COUNT + matches))
    fi
done

if [ "$JOYUE_COUNT" -eq 0 ]; then
    echo "   未找到 JOYUE 运行日志"
    echo "   提示: 如果 AutoDeployEnabled=false，不会产生 JOYUE 运行日志"
fi
echo ""

# 4. 专门查找 [JOYUE] 格式的日志
echo "4. 查找 [JOYUE] 格式的日志："
JOYUE_BRACKET_COUNT=0
find "$LATEST_LOG_DIR" -name "*.log" -type f 2>/dev/null | while read -r logfile; do
    matches=$(grep -c "\[JOYUE\]" "$logfile" 2>/dev/null || echo "0")
    matches=$(echo "$matches" | awk '{print $1}')
    if [ -n "$matches" ] && [ "$matches" -gt 0 ] 2>/dev/null; then
        echo ""
        echo "   === $(basename "$logfile") (找到 $matches 条) ==="
        grep "\[JOYUE\]" "$logfile" 2>/dev/null | head -20
        JOYUE_BRACKET_COUNT=$((JOYUE_BRACKET_COUNT + matches))
    fi
done

if [ "$JOYUE_BRACKET_COUNT" -eq 0 ]; then
    echo "   未找到 [JOYUE] 格式的日志"
fi
echo ""

# 5. 查找错误级别的 JOYUE 日志
echo "5. 查找 JOYUE 错误日志："
find "$LATEST_LOG_DIR" -name "*.log" -type f 2>/dev/null | while read -r logfile; do
    # 在 JSON 格式中查找 level=error 且包含 joyue 的日志
    grep -i "joyue" "$logfile" 2>/dev/null | grep -i "error" | head -10 | while IFS= read -r line; do
        if echo "$line" | grep -q "^{"; then
            if command -v python3 >/dev/null 2>&1; then
                formatted=$(echo "$line" | python3 -m json.tool 2>/dev/null)
                if [ $? -eq 0 ]; then
                    echo "$formatted" | head -10
                else
                    echo "   $line"
                fi
            else
                echo "   $line"
            fi
        else
            echo "   $line"
        fi
    done
done
echo ""

# 6. 检查 JOYUE 配置
echo "6. 检查 JOYUE 配置（从日志中）："
find "$LATEST_LOG_DIR" -name "*.log" -type f 2>/dev/null | head -1 | xargs grep -i "joyue" 2>/dev/null | grep -i "config\|auto.*deploy\|deploy.*key" | head -5
echo ""

# 7. 统计信息
echo "7. 统计信息："
# 排除配置中的 JOYUE 字段
TOTAL_JOYUE=$(find "$LATEST_LOG_DIR" -name "*.log" -type f -exec grep -i "joyue" {} \; 2>/dev/null | grep -v '"Joyue":' | grep -v '"joyue":' | grep -E '\[JOYUE\]|"message".*[Jj][Oo][Yy][Uu][Ee]' | wc -l | tr -d ' ')
TOTAL_ERROR=$(find "$LATEST_LOG_DIR" -name "*.log" -type f -exec grep -i "joyue" {} \; 2>/dev/null | grep -v '"Joyue":' | grep -v '"joyue":' | grep -iE 'error.*joyue|joyue.*error' | wc -l | tr -d ' ')
TOTAL_BRACKET=$(find "$LATEST_LOG_DIR" -name "*.log" -type f -exec grep -c "\[JOYUE\]" {} \; 2>/dev/null | awk '{sum+=$1} END {print sum+0}')
echo "   JOYUE 运行日志数: $TOTAL_JOYUE"
echo "   [JOYUE] 格式日志数: $TOTAL_BRACKET"
echo "   JOYUE 错误日志数: $TOTAL_ERROR"
echo ""

# 8. 提供搜索建议
echo "8. 如果未找到日志，可能的原因："
echo "   - AutoDeployEnabled 未启用（检查配置中的 Joyue.AutoDeployEnabled）"
echo "   - 日志级别过低（需要 verbosity >= 3 才能看到 Info 级别日志）"
echo "   - 主合约尚未部署（JOYUE 日志只在检测到主合约部署时产生）"
echo "   - 日志文件过大，grep 可能较慢"
echo ""
echo "   手动搜索命令："
echo "   grep -r 'JOYUE' $LATEST_LOG_DIR --include='*.log' | head -50"
echo "   grep -r 'joyue' $LATEST_LOG_DIR --include='*.log' -i | head -50"

