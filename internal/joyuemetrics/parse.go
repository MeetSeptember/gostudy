package joyuemetrics

import (
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// ParseKv 解析 key=value,key2=value2（用于 --rpcs）。
func ParseKv(s string) map[string]string {
	m := make(map[string]string)
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx := strings.Index(p, "=")
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(p[:idx])
		v := strings.TrimSpace(p[idx+1:])
		if k != "" && v != "" {
			m[k] = v
		}
	}
	return m
}

// ParseAddrMap 单地址/分片（与历史 joyue-metrics 一致）。
func ParseAddrMap(s string) map[string]common.Address {
	m := make(map[string]common.Address)
	s = strings.TrimSpace(s)
	if s == "" {
		return m
	}
	if !strings.Contains(s, "=") {
		addr := common.HexToAddress(s)
		m["0"] = addr
		return m
	}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx := strings.Index(p, "=")
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(p[:idx])
		v := strings.TrimSpace(p[idx+1:])
		if k != "" && v != "" {
			m[k] = common.HexToAddress(v)
		}
	}
	return m
}

// ParseMultiAddrMap 每分片可多地址，分片内用分号分隔：
//
//	0=0xAAA;0xBBB,1=0xCCC
//	0xDDD  （无 = 时等价于 0=0xDDD）
//
// 与单地址格式兼容：0=0xAAA,1=0xBBB 每分片仅一个地址。
func ParseMultiAddrMap(s string) map[string][]common.Address {
	out := make(map[string][]common.Address)
	s = strings.TrimSpace(s)
	if s == "" {
		return out
	}
	if !strings.Contains(s, "=") {
		addr := common.HexToAddress(s)
		out["0"] = []common.Address{addr}
		return out
	}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx := strings.Index(p, "=")
		if idx < 0 {
			continue
		}
		k := strings.TrimSpace(p[:idx])
		v := strings.TrimSpace(p[idx+1:])
		if k == "" || v == "" {
			continue
		}
		var addrs []common.Address
		for _, part := range strings.Split(v, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			addrs = append(addrs, common.HexToAddress(part))
		}
		if len(addrs) > 0 {
			out[k] = addrs
		}
	}
	return out
}

// SingleAddrMapToMulti 将 v1 单地址 map 转为多地址 map（每分片一项）。
func SingleAddrMapToMulti(m map[string]common.Address) map[string][]common.Address {
	out := make(map[string][]common.Address, len(m))
	for k, v := range m {
		if v != (common.Address{}) {
			out[k] = []common.Address{v}
		}
	}
	return out
}
