# no shebang; to be sourced from other scripts

unset -v progdir
case "${0}" in
*/*) progdir="${0%/*}";;
*) progdir=.;;
esac

case "${HMY_PATH+set}" in
"")
   unset -v gopath
   gopath=$(go env GOPATH)
   # HMY_PATH is the common root directory of all harmony repos
   HMY_PATH="${gopath%%:*}/src/github.com/harmony-one"
   if [ ! -d $HMY_PATH ]; then
      # "env pwd" uses external pwd(1) implementation and not the Bash built-in,
      # which does not fully dereference symlinks.
      HMY_PATH=$(cd $progdir/../.. && env pwd)
   fi
   ;;
esac
# macOS + Homebrew：新版常无 openssl@1.1，未设置 OPENSSL_DIR 时自动探测，避免 ld: library 'crypto' not found。
# Linux 不设 OPENSSL_DIR（与原先一致）；任意系统都可用 export OPENSSL_DIR=... 覆盖。
if [ "$(uname -s)" = "Darwin" ] && [ -z "${OPENSSL_DIR}" ]; then
	if [ -d "/opt/homebrew/opt/openssl@3" ]; then
		OPENSSL_DIR="/opt/homebrew/opt/openssl@3"
	elif [ -d "/opt/homebrew/opt/openssl@1.1" ]; then
		OPENSSL_DIR="/opt/homebrew/opt/openssl@1.1"
	elif [ -d "/usr/local/opt/openssl@3" ]; then
		OPENSSL_DIR="/usr/local/opt/openssl@3"
	elif [ -d "/usr/local/opt/openssl@1.1" ]; then
		OPENSSL_DIR="/usr/local/opt/openssl@1.1"
	fi
fi
[ -n "${OPENSSL_DIR}" ] && export OPENSSL_DIR
# BLS 链接依赖 libgmp（与根 Makefile 中 LD_LIBRARY_PATH 含 gmp 一致）；Darwin 上需 -L 才能在链接阶段找到。
if [ "$(uname -s)" = "Darwin" ] && [ -z "${GMP_DIR}" ]; then
	if [ -d "/opt/homebrew/opt/gmp" ]; then
		GMP_DIR="/opt/homebrew/opt/gmp"
	elif [ -d "/usr/local/opt/gmp" ]; then
		GMP_DIR="/usr/local/opt/gmp"
	fi
fi
[ -n "${GMP_DIR}" ] && export GMP_DIR
: ${MCL_DIR="${HMY_PATH}/mcl"}
: ${BLS_DIR="${HMY_PATH}/bls"}
export CGO_CFLAGS="-I${BLS_DIR}/include -I${MCL_DIR}/include"
export CGO_LDFLAGS="-L${BLS_DIR}/lib"
export LD_LIBRARY_PATH=${BLS_DIR}/lib:${MCL_DIR}/lib

OS=$(uname -s)
case $OS in
   Darwin)
      if [ -n "${OPENSSL_DIR}" ] && [ -d "${OPENSSL_DIR}/include" ]; then
         export CGO_CFLAGS="-I${BLS_DIR}/include -I${MCL_DIR}/include -I${OPENSSL_DIR}/include"
         export CGO_LDFLAGS="-L${BLS_DIR}/lib -L${OPENSSL_DIR}/lib"
         export LD_LIBRARY_PATH=${BLS_DIR}/lib:${MCL_DIR}/lib:${OPENSSL_DIR}/lib
      else
         echo "setup_bls_build_flags.sh: 未找到 OpenSSL（请先: brew install openssl@3，或 export OPENSSL_DIR=...）" >&2
         export CGO_CFLAGS="-I${BLS_DIR}/include -I${MCL_DIR}/include"
         export CGO_LDFLAGS="-L${BLS_DIR}/lib"
         export LD_LIBRARY_PATH=${BLS_DIR}/lib:${MCL_DIR}/lib
      fi
      if [ -n "${GMP_DIR}" ] && [ -d "${GMP_DIR}/lib" ]; then
         export CGO_CFLAGS="${CGO_CFLAGS} -I${GMP_DIR}/include"
         export CGO_LDFLAGS="${CGO_LDFLAGS} -L${GMP_DIR}/lib"
         export LD_LIBRARY_PATH="${LD_LIBRARY_PATH}:${GMP_DIR}/lib"
      else
         echo "setup_bls_build_flags.sh: 未找到 GMP（请先: brew install gmp，或 export GMP_DIR=...）" >&2
      fi
      export DYLD_FALLBACK_LIBRARY_PATH=$LD_LIBRARY_PATH
      ;;
esac

if [ "$1" = "-v" ]; then
   echo "{ \"CGO_CFLAGS\" : \"$CGO_CFLAGS\",
            \"CGO_LDFLAGS\" : \"$CGO_LDFLAGS\",
            \"LD_LIBRARY_PATH\" : \"$LD_LIBRARY_PATH\",
            \"DYLD_FALLBACK_LIBRARY_PATH\" : \"$DYLD_FALLBACK_LIBRARY_PATH\"}" | jq "."
fi
