// genlocalnetkeys 在起链前批量生成 localnet 用的 BLS 密钥与 genesis/deploy 片段。
//
// 用法（在仓库根目录执行）:
//
//	go run ./cmd/genlocalnetkeys -count 24 -keydir .hmy -pass ""
//
// 会：在 -keydir 下写入 <BLS公钥hex>.key（口令与 -pass 一致，空口令对齐 deploy 默认 blspass.txt），
// 并在 stdout 打印可粘贴到 internal/genesis/localnodes.go 的 DeployAccount 行，
// 以及类似 test/configs/local-resharding.txt 的 validator 行（不含 explorer，需按分片自行补）。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/harmony-one/harmony/crypto/bls"
	"github.com/harmony-one/harmony/internal/blsgen"
	internalcommon "github.com/harmony-one/harmony/internal/common"
	"github.com/harmony-one/harmony/internal/utils"
)

func main() {
	var (
		count    = flag.Int("count", 0, "生成的密钥条数（必填，>0）")
		keyDir   = flag.String("keydir", ".hmy", "写入 .key 的目录（与 test/deploy.sh 中 --blspass 及配置里 .hmy/ 路径一致）")
		pass     = flag.String("pass", "", "BLS 密钥加密口令，需与 .hmy/blspass.txt 一致（空串表示无口令）")
		baseIP   = flag.String("ip", "127.0.0.1", "deploy 配置里的 IP")
		basePort = flag.Int("base-port", 9000, "第一条 validator 的 P2P 端口")
		portStep = flag.Int("port-step", 4, "相邻 validator 的端口间隔（与现有 local-resharding 一致为 4）")
		printGo  = flag.Bool("print-go", true, "是否打印 genesis.DeployAccount 风格的 Go 片段")
		printDep = flag.Bool("print-deploy", true, "是否打印 deploy 用的 validator 行")
	)
	flag.Parse()
	if *count <= 0 {
		fmt.Fprintln(os.Stderr, "必须指定 -count > 0，例如: go run ./cmd/genlocalnetkeys -count 24 -keydir .hmy")
		os.Exit(2)
	}

	keyDirAbs, err := filepath.Abs(*keyDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(keyDirAbs, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Chdir(keyDirAbs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.Chdir(cwd) }()

	fmt.Fprintf(os.Stderr, "[genlocalnetkeys] writing %d keys under %s\n", *count, keyDirAbs)

	type row struct {
		index int
		one   string
		bls   string
		fname string
		port  int
	}
	rows := make([]row, 0, *count)

	for i := 0; i < *count; i++ {
		sk, fname, err := blsgen.GenBLSKeyWithPassPhrase(*pass)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		pub := sk.GetPublicKey()
		ser := bls.FromLibBLSPublicKeyUnsafe(pub)
		addr := utils.GetAddressFromBLSPubKeyBytes(ser.Bytes())
		one := internalcommon.MustAddressToBech32(addr)
		blsHex := pub.SerializeToHexStr()
		if blsHex != fname[:len(fname)-len(".key")] {
			// 文件名由 blsgen 按公钥 hex 命名，理论上应一致
			fmt.Fprintf(os.Stderr, "warn: fname %q vs hex %q\n", fname, blsHex)
		}
		port := *basePort + i*(*portStep)
		rows = append(rows, row{index: i, one: one, bls: blsHex, fname: fname, port: port})
	}

	relKeyPath := func(fname string) string {
		base := filepath.Base(fname)
		// deploy 在仓库根执行时用 .hmy/xxx.key；与 -keydir 最后一级目录名一致（一般为 .hmy）
		return filepath.ToSlash(filepath.Join(filepath.Base(keyDirAbs), base))
	}

	if *printGo {
		fmt.Println("// 粘贴到 internal/genesis/localnodes.go 中对应 var（例如 LocalHarmonyAccountsV2）末尾：")
		for _, r := range rows {
			idx := strconv.Itoa(r.index)
			fmt.Printf("\t{Index: \" %s \", Address: %q, BLSPublicKey: %q},\n", idx, r.one, r.bls)
		}
		fmt.Println()
	}

	if *printDep {
		fmt.Println("# 粘贴到 test/configs/*.txt 的 validator 段（路径按 -keydir 相对仓库根调整）：")
		for _, r := range rows {
			fmt.Printf("%s %d validator %s\n", *baseIP, r.port, relKeyPath(r.fname))
		}
		fmt.Println()
	}

	fmt.Fprintln(os.Stderr, "[genlocalnetkeys] done.")
}
