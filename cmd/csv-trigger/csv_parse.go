package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// addressPair 表示 CSV 中一行解析出的「两端地址」（如转账 from/to；后续场景可复用列名映射）。
type addressPair struct {
	A common.Address
	B common.Address
}

// parseAddressPairCSV 读取含「起点列 + 终点列」的 CSV；列名支持 sender/from 与 target/to（不区分大小写）。
func parseAddressPairCSV(path string) ([]addressPair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		return nil, err
	}
	iA, iB, err := resolvePairColumnIndices(header)
	if err != nil {
		return nil, err
	}

	var out []addressPair
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) == 0 {
			continue
		}
		sa := strings.TrimSpace(csvCell(rec, iA))
		sb := strings.TrimSpace(csvCell(rec, iB))
		if sa == "" && sb == "" {
			continue
		}
		if !common.IsHexAddress(sa) || !common.IsHexAddress(sb) {
			return nil, fmt.Errorf("invalid address row: colA=%q colB=%q", sa, sb)
		}
		out = append(out, addressPair{A: common.HexToAddress(sa), B: common.HexToAddress(sb)})
	}
	return out, nil
}

func csvCell(rec []string, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return rec[i]
}

func resolvePairColumnIndices(header []string) (iA, iB int, err error) {
	iA, iB = -1, -1
	for i, h := range header {
		key := strings.ToLower(strings.TrimSpace(h))
		switch key {
		case "sender", "from":
			iA = i
		case "target", "to":
			iB = i
		}
	}
	if iA < 0 {
		return -1, -1, fmt.Errorf("csv header: need column sender or from, got %v", header)
	}
	if iB < 0 {
		return -1, -1, fmt.Errorf("csv header: need column target or to, got %v", header)
	}
	return iA, iB, nil
}

// parseSenderColumnCSV 读取 AMM 2PC、wallet-mevarb-2pc、wallet-mevarb-chainspace、wallet-mevarb-sparrow、wallet-mevarb-joyue 等「单列用户」CSV：表头须含 sender、from 或 address 之一（取最先匹配的列）。
// 返回的 addressPair 中 A 为 user，B 与 A 相同（占位，encode 仅使用 A）。
func parseSenderColumnCSV(path string) ([]addressPair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		return nil, err
	}
	iU, err := resolveAmmUserColumnIndex(header)
	if err != nil {
		return nil, err
	}

	var out []addressPair
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) == 0 {
			continue
		}
		su := strings.TrimSpace(csvCell(rec, iU))
		if su == "" {
			continue
		}
		if !common.IsHexAddress(su) {
			return nil, fmt.Errorf("invalid user address %q", su)
		}
		a := common.HexToAddress(su)
		out = append(out, addressPair{A: a, B: a})
	}
	return out, nil
}

func resolveAmmUserColumnIndex(header []string) (int, error) {
	for _, key := range []string{"sender", "from", "address"} {
		for i, h := range header {
			if strings.EqualFold(strings.TrimSpace(h), key) {
				return i, nil
			}
		}
	}
	return -1, fmt.Errorf("csv header: need one of sender, from, address in %v", header)
}
