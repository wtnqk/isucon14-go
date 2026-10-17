#!/usr/bin/env bash
# コミット済みのアプリをサーバーへ送り、サーバー上でビルドして再起動する。
#
#   scripts/deploy.sh app1                 # HEAD を app1 へ
#   scripts/deploy.sh app1 app2 app3       # 複数台へ（途中で失敗したら、残りの台には送らない）
#   scripts/deploy.sh --rev abc1234 app1   # 指定したコミットへ戻す（ロールバック）
#   scripts/deploy.sh --build-only app1    # ビルドが通るかだけ試す（反映・再起動しない）
#
# ホストは ssh の接続先。"-" を含まない名前には DEPLOY_HOST_PREFIX を付ける（app1 -> isucon14-app1）。
#
# 流れ（1台ごと）:
#   1. git archive で指定コミットの DEPLOY_PATHS を固め、ssh でステージングに展開
#   1.5 サーバーの REMOTE_OVERLAY にあるファイルを、ステージングへ上書きコピー
#       （git に入れない計測用コード（pprof など）をサーバーごとに足すため。
#         例: REMOTE_OVERLAY/webapp/go/pprotein_debug.go -> webapp/go/ に入る。外すときは消してデプロイ）
#   2. ステージングでビルド。失敗したら、動いているアプリには触らない
#   3. ステージングを REMOTE_ROOT へ rsync --delete で反映し、REVISION を書く
#   4. APP_SERVICE を再起動し、起動と HTTP 応答を確認
set -euo pipefail

# ---- 設定（環境変数で上書きできる）----
DEPLOY_HOST_PREFIX="${DEPLOY_HOST_PREFIX:-isucon14-}"
DEPLOY_PATHS="${DEPLOY_PATHS:-webapp/go webapp/sql}"
REMOTE_ROOT="${REMOTE_ROOT:-/home/isucon/webapp}"       # DEPLOY_PATHS の "webapp/" 以下がここへ入る
REMOTE_STAGE="${REMOTE_STAGE:-/home/isucon/.deploy/stage}"
REMOTE_OVERLAY="${REMOTE_OVERLAY:-/home/isucon/.deploy/overlay}"
REMOTE_GO="${REMOTE_GO:-/home/isucon/local/golang/bin/go}"
APP_SERVICE="${APP_SERVICE:-isuride-go.service}"
APP_BIN="${APP_BIN:-isuride}"
# 認証なしで叩いて 401 が返れば生きている。POST /api/initialize は DB を初期化するので使わない。
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080/api/app/notification}"

usage() {
    sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//' >&2
    exit 1
}

rev="HEAD"
build_only=0
hosts=()
while [ $# -gt 0 ]; do
    case "$1" in
        --rev) rev="${2:?--rev にはコミットを指定する}"; shift 2 ;;
        --build-only) build_only=1; shift ;;
        -h|--help) usage ;;
        -*) echo "不明なオプション: $1" >&2; usage ;;
        *) hosts+=("$1"); shift ;;
    esac
done
[ ${#hosts[@]} -gt 0 ] || usage

cd "$(git rev-parse --show-toplevel)"

# 送るのはコミット済みの内容だけ。HEAD を送るときに未コミットの変更があれば止める
# （残したまま送ると、サーバーの中身がどのコミットか分からなくなる）。
# --build-only は反映しないので、未コミットの作業中でも試せるようにする。
# shellcheck disable=SC2086
if [ "$rev" = "HEAD" ] && [ $build_only -eq 0 ] && [ -n "$(git status --porcelain -- $DEPLOY_PATHS)" ]; then
    echo "!! 未コミットの変更があります。コミットしてからデプロイしてください:" >&2
    # shellcheck disable=SC2086
    git status --short -- $DEPLOY_PATHS >&2
    exit 1
fi

commit="$(git rev-parse --verify "${rev}^{commit}")"
short="$(git rev-parse --short "$commit")"
subject="$(git log -1 --format=%s "$commit")"
deployer="$(git config user.name || whoami)"

# 他の人が同じコミットを再現できるよう、push 済みかを確かめる（止めはしない）
if [ -z "$(git branch -r --contains "$commit" 2>/dev/null)" ]; then
    echo "   注意: $short はまだ push されていません"
fi

echo "==> デプロイ: $short $subject"

for h in "${hosts[@]}"; do
    case "$h" in
        *-*) host="$h" ;;
        *) host="${DEPLOY_HOST_PREFIX}${h}" ;;
    esac
    echo ""
    echo "==> [$host] 送信"
    # shellcheck disable=SC2086
    if [ $build_only -eq 1 ]; then
        # 作業ツリーをそのまま送る（未コミットの変更も含む。追跡していないファイルも含めるため tar を使う）
        # macOS の tar が付ける ._ ファイルと拡張属性は、Linux 側で邪魔になるので付けない
        COPYFILE_DISABLE=1 tar --no-xattrs --no-fflags --exclude="$APP_BIN" -cf - $DEPLOY_PATHS
    else
        git archive --format=tar "$commit" $DEPLOY_PATHS
    fi | ssh "$host" "set -e; rm -rf '$REMOTE_STAGE'; mkdir -p '$REMOTE_STAGE'; tar -xf - -C '$REMOTE_STAGE'"

    if [ $build_only -eq 1 ]; then
        echo "==> [$host] ビルドのみ"
        ssh "$host" bash -s -- "$REMOTE_STAGE" "$REMOTE_OVERLAY" "$REMOTE_GO" "$APP_BIN" <<'REMOTE'
set -euo pipefail
stage=$1 overlay=$2 go=$3 bin=$4
if [ -d "$overlay" ] && [ -n "$(ls -A "$overlay")" ]; then
    echo "   overlay から追加: $(cd "$overlay" && find . -type f | sed 's|^\./||' | tr '\n' ' ')"
    cp -a "$overlay/." "$stage/"
fi
cd "$stage/webapp/go"
"$go" build -o "$bin" .
echo "   OK: ビルド成功（反映・再起動はしていない）"
REMOTE
        continue
    fi

    echo "==> [$host] ビルド → 反映 → 再起動"
    ssh "$host" bash -s -- \
        "$REMOTE_STAGE" "$REMOTE_ROOT" "$REMOTE_GO" "$APP_BIN" "$APP_SERVICE" "$HEALTH_URL" \
        "$short" "$subject" "$deployer" "$DEPLOY_PATHS" "$REMOTE_OVERLAY" <<'REMOTE'
set -euo pipefail
stage=$1 root=$2 go=$3 bin=$4 svc=$5 health=$6 short=$7 subject=$8 deployer=$9 paths=${10} overlay=${11}

# rsync --delete の宛先を間違えると取り返しがつかないので、最初に確かめる
[ -n "$root" ] && [ -d "$root/go" ] || { echo "!! REMOTE_ROOT が不正: '$root'" >&2; exit 1; }

# git に入れないサーバー固有のファイル（計測用コードなど）を足す
overlay_files=""
if [ -d "$overlay" ] && [ -n "$(ls -A "$overlay")" ]; then
    overlay_files="$(cd "$overlay" && find . -type f | sed 's|^\./||' | tr '\n' ' ')"
    echo "   overlay から追加: $overlay_files"
    cp -a "$overlay/." "$stage/"
fi

cd "$stage/webapp/go"
"$go" build -o "$bin" .

for p in $paths; do
    sub="${p#webapp/}"
    rsync -a --delete "$stage/$p/" "$root/$sub/"
done
printf '%s %s\n%s by %s\noverlay: %s\n' "$short" "$subject" "$(date '+%F %T')" "$deployer" "${overlay_files:-なし}" > "$root/REVISION"

sudo systemctl restart "$svc"
sleep 1
if ! systemctl is-active --quiet "$svc"; then
    echo "!! $svc が起動していません" >&2
    sudo journalctl -u "$svc" -n 30 --no-pager >&2
    exit 1
fi
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$health" || true)"
if [ "$code" = "000" ] || [ "$code" -ge 500 ]; then
    echo "!! HTTP 応答が異常: $health -> $code" >&2
    sudo journalctl -u "$svc" -n 30 --no-pager >&2
    exit 1
fi
echo "   OK: $svc active, $health -> $code"
REMOTE
done

echo ""
if [ $build_only -eq 1 ]; then
    echo "==> 完了: ${hosts[*]} でビルドを確認（反映はしていない）"
else
    echo "==> 完了: $short を ${hosts[*]} へ"
fi
