#!/usr/bin/env bash
# コミット済みのアプリをサーバーへ送り、サーバー上でビルドして再起動する。
#
#   scripts/deploy.sh app1                 # HEAD を app1 へ（役割は app とみなす）
#   scripts/deploy.sh app1 app3            # 複数台へ（途中で失敗したら、残りの台には送らない）
#   scripts/deploy.sh --rev abc1234 app1   # 指定したコミットへ戻す（ロールバック）
#   scripts/deploy.sh --build-only app1    # ビルドが通るかだけ試す（反映・再起動しない）
#   scripts/deploy.sh app1:app app3:db     # 台ごとに役割を指定する
#
# ホストは ssh の接続先。"-" を含まない名前には DEPLOY_HOST_PREFIX を付ける（app1 -> isucon14-app1）。
#
# 役割（HOST:ROLE。省略時は app）:
#   app    webapp/go を送ってビルドし、APP_SERVICE を再起動する
#   db     webapp/sql を送るだけ。WEB_SERVICE を再起動する（nginx など。空なら再起動しない）
#   nginx  nginx 本体の nginx.conf と DOMAIN の isuride.conf を送って nginx を再起動する
#
# 3台構成の例:
#   scripts/deploy.sh app1:app app3:db app2:nginx
#
# 流れ（1台ごと）:
#   1. git archive で指定コミットの対象パスを固め、ssh でステージングに展開
#   1.5 サーバーの REMOTE_OVERLAY にあるファイルを、ステージングへ上書きコピー
#       （git に入れない計測用コード（pprof など）をサーバーごとに足すため。
#         例: REMOTE_OVERLAY/webapp/go/pprotein_debug.go -> webapp/go/ に入る。外すときは消してデプロイ）
#   2. app はステージングでビルド。失敗したら、動いているアプリには触らない
#   3. ステージングを REMOTE_ROOT へ rsync --delete で反映し、REVISION を書く
#   4. app/db は APP_SERVICE / WEB_SERVICE を再起動し、起動と HTTP 応答を確認
set -euo pipefail

# ---- 設定（環境変数で上書きできる）----
DEPLOY_HOST_PREFIX="${DEPLOY_HOST_PREFIX:-isucon14-}"
REMOTE_ROOT="${REMOTE_ROOT:-/home/isucon/webapp}"       # 対象パスの "webapp/" 以下がここへ入る
REMOTE_STAGE="${REMOTE_STAGE:-/home/isucon/.deploy/stage}"
REMOTE_OVERLAY="${REMOTE_OVERLAY:-/home/isucon/.deploy/overlay}"
REMOTE_GO="${REMOTE_GO:-/home/isucon/local/golang/bin/go}"
APP_SERVICE="${APP_SERVICE:-isuride-go.service}"
APP_BIN="${APP_BIN:-isuride}"
# 認証なしで叩いて 401 が返れば生きている。POST /api/initialize は DB を初期化するので使わない。
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080/api/app/notification}"
# nginx の server_name と、db の後に再起動するサービス（db 台で nginx も動かす時だけ使う）
DOMAIN="${DOMAIN:-xiv.isucon.net}"
DB_WEB_SERVICE="${DB_WEB_SERVICE:-}"

# 役割ごとの設定
deploy_paths_for() {
    case "$1" in
        app) echo "webapp/go webapp/sql" ;;   # sql は POST /api/initialize が app 台で exec するため要る
        db) echo "webapp/sql" ;;
        nginx) echo "" ;;                     # 設定ファイルは別途送る
        *) return 1 ;;
    esac
}

# ssh は引数をつなげて1つのコマンド文字列としてサーバーのシェルに渡す。
# スペースを含む値（コミットメッセージなど）が分割されないよう、引用符で囲んでから渡す。
remote_bash() {
    local host="$1"
    shift
    ssh "$host" "bash -s -- $(printf '%q ' "$@")"
}

usage() {
    sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//' >&2
    exit 1
}

rev="HEAD"
build_only=0
targets=()
while [ $# -gt 0 ]; do
    case "$1" in
        --rev) rev="${2:?--rev にはコミットを指定する}"; shift 2 ;;
        --build-only) build_only=1; shift ;;
        -h|--help) usage ;;
        -*) echo "不明なオプション: $1" >&2; usage ;;
        *) targets+=("$1"); shift ;;
    esac
done
[ ${#targets[@]} -gt 0 ] || usage

cd "$(git rev-parse --show-toplevel)"

# 役割が不正なまま先へ進むと、意図しない台に意図しないものを送ってしまうので最初に全部検証する
declare -a host_names=() roles=()
for t in "${targets[@]}"; do
    name="${t%%:*}"
    role="${t#*:}"
    [ "$role" = "$t" ] && role="app"
    if ! deploy_paths_for "$role" >/dev/null; then
        echo "!! 不明な役割: '$role'（app / db / nginx のいずれか）" >&2
        usage
    fi
    host_names+=("$name")
    roles+=("$role")
done

# 送るのはコミット済みの内容だけ。HEAD を送るときに未コミットの変更があれば止める
# （残したまま送ると、サーバーの中身がどのコミットか分からなくなる）。
# --build-only は反映しないので、未コミットの作業中でも試せるようにする。
if [ "$rev" = "HEAD" ] && [ $build_only -eq 0 ]; then
    if [ -n "$(git status --porcelain)" ]; then
        echo "!! 未コミットの変更があります。コミットしてからデプロイしてください:" >&2
        git status --short >&2
        exit 1
    fi
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

for i in "${!host_names[@]}"; do
    n="${host_names[$i]}"
    role="${roles[$i]}"
    case "$n" in
        *-*) host="$n" ;;
        *) host="${DEPLOY_HOST_PREFIX}${n}" ;;
    esac
    paths="$(deploy_paths_for "$role")"
    echo ""
    echo "==> [$host] $role"
    # nginx 役割は webapp 配下に送るものが無いので、ステージングだけ用意する
    if [ -n "$paths" ]; then
        # shellcheck disable=SC2086
        if [ $build_only -eq 1 ]; then
            # 作業ツリーをそのまま送る（未コミットの変更も含む。追跡していないファイルも含めるため tar を使う）
            # macOS の tar が付ける ._ ファイルと拡張属性は、Linux 側で邪魔になるので付けない
            COPYFILE_DISABLE=1 tar --no-xattrs --no-fflags --exclude="$APP_BIN" -cf - $paths
        else
            git archive --format=tar "$commit" $paths
        fi | ssh "$host" "set -e; rm -rf '$REMOTE_STAGE'; mkdir -p '$REMOTE_STAGE'; tar -xf - -C '$REMOTE_STAGE'"
    else
        ssh "$host" "set -e; rm -rf '$REMOTE_STAGE'; mkdir -p '$REMOTE_STAGE'"
    fi

    # nginx の設定はリポジトリの構成情報なので、対象パスとは別に送る
    # --build-only は未コミットの内容も試せるので、そのときは作業ツリーから固める
    if [ "$role" = "nginx" ]; then
        echo "   送信: nginx 設定 (server_name ${DOMAIN})"
        if [ $build_only -eq 1 ]; then
            COPYFILE_DISABLE=1 tar --no-xattrs --no-fflags -cf - provisioning/ansible/roles/nginx/files/etc/nginx
        else
            git archive --format=tar "$commit" provisioning/ansible/roles/nginx/files/etc/nginx
        fi | ssh "$host" "set -e; tar -xf - -C '$REMOTE_STAGE'"
    fi

    if [ $build_only -eq 1 ]; then
        if [ "$role" != "app" ]; then
            echo "   OK: $role は反映物の送信のみ（ビルドなし）"
            continue
        fi
        echo "==> [$host] ビルドのみ"
        remote_bash "$host" "$REMOTE_STAGE" "$REMOTE_OVERLAY" "$REMOTE_GO" "$APP_BIN" <<'REMOTE'
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

    echo "==> [$host] 反映 → 再起動"
    remote_bash "$host" \
        "$REMOTE_STAGE" "$REMOTE_ROOT" "$REMOTE_GO" "$APP_BIN" "$APP_SERVICE" "$HEALTH_URL" \
        "$short" "$subject" "$deployer" "$paths" "$REMOTE_OVERLAY" "$role" \
        "$DOMAIN" "$DB_WEB_SERVICE" <<'REMOTE'
set -euo pipefail
stage=$1 root=$2 go=$3 bin=$4 app_svc=$5 health=$6 short=$7 subject=$8 deployer=$9 paths=${10} overlay=${11} role=${12} domain=${13} db_web_svc=${14}

# rsync --delete の宛先を間違えると取り返しがつかないので、最初に確かめる
[ -n "$root" ] && [ -d "$root" ] || { echo "!! REMOTE_ROOT が不正: '$root'" >&2; exit 1; }

# nginx の設定は反映先が /home/isucon/webapp ではないので、ここで別に扱う
if [ "$role" = "nginx" ]; then
    # provisioning/... ごと固めて展開しているので、リポジトリ内の配置をそのまま辿る
    # 送るのは本体の nginx.conf と isuride.conf の 2 つ
    files="$stage/provisioning/ansible/roles/nginx/files"
    conf="$files/etc/nginx/sites-available/isuride.conf"
    main_conf="$files/etc/nginx/nginx.conf"
    for f in "$conf" "$main_conf"; do
        if [ ! -f "$f" ]; then
            echo "!! nginx 設定がステージングに無い: $f" >&2
            exit 1
        fi
    done
    target=/etc/nginx/sites-available/isuride.conf
    main_target=/etc/nginx/nginx.conf
    checkdir="/home/isucon/.deploy/nginx-check"

    # 1. 稼働中の /etc/nginx のコピーを作り、そこに候補の設定を入れて検証する。
    #    ここでは本物の設定に触らないので、検証に落ちても稼働中の nginx は無傷。
    #    sites-enabled から sites-available へのリンクはコピー先を指すよう張り直す。
    sudo rm -rf "$checkdir"
    sudo mkdir -p "$checkdir"
    sudo cp -a /etc/nginx/. "$checkdir/"
    sudo cp "$main_conf" "$checkdir/nginx.conf"
    sudo rm -f "$checkdir/sites-enabled/isuride.conf"
    sudo cp "$conf" "$checkdir/sites-available/isuride.conf"
    # リンクは消すのではなく、コピー先の設定を指すように張り直す（消すと検証対象が読み込まれない）
    sudo ln -sfn "$checkdir/sites-available/isuride.conf" "$checkdir/sites-enabled/isuride.conf"
    # DOMAIN が既定と違う時だけ server_name を差し替える（証明書のパスはそのまま）
    if [ "$domain" != "xiv.isucon.net" ]; then
        sudo sed -i "s/xiv\.isucon\.net/$domain/g" "$checkdir/sites-available/isuride.conf"
    fi
    # /etc/nginx 前提のパスを検証用ディレクトリへ読み替える
    sudo sed -i "s|/etc/nginx|$checkdir|g" "$checkdir/nginx.conf"
    # パイプの終了コードは tail のものになるので、nginx の終了コードはファイルに残して見る
    tlog=/tmp/nginx-check-deploy.log
    sudo nginx -t -c "$checkdir/nginx.conf" >"$tlog" 2>&1 || true
    tail -3 "$tlog"
    if ! grep -q "test is successful" "$tlog"; then
        rm -f "$tlog"
        sudo rm -rf "$checkdir"
        echo "!! nginx 設定が不正。反映していない（稼働中の設定はそのまま）" >&2
        exit 1
    fi
    rm -f "$tlog"

    # 2. 検証を通った候補だけを本物の設定にする。旧設定は退避しておき、失敗したら戻す
    #    検証用の nginx.conf はパスを読み替えているので、本物には送られてきたものを置く
    suffix=".bak-deploy-$(date +%Y%m%d%H%M%S)"
    sudo cp -a "$target" "$target$suffix"
    sudo cp -a "$main_target" "$main_target$suffix"
    sudo cp "$checkdir/sites-available/isuride.conf" "$target"
    sudo cp "$main_conf" "$main_target"
    if ! sudo nginx -t; then
        sudo cp -a "$target$suffix" "$target"
        sudo cp -a "$main_target$suffix" "$main_target"
        sudo rm -rf "$checkdir"
        echo "!! 配置後の nginx -t が失敗したので $target$suffix と $main_target$suffix から戻した" >&2
        exit 1
    fi
    sudo rm -rf "$checkdir"
    sudo systemctl reload nginx
    printf '%s %s\n%s by %s\nrole: %s\n' "$short" "$subject" "$(date '+%F %T')" "$deployer" "$role" > "$root/REVISION"
    echo "   OK: nginx 設定を反映 ($domain)"
    exit 0
fi

# git に入れないサーバー固有のファイル（計測用コードなど）を足す
overlay_files=""
if [ -d "$overlay" ] && [ -n "$(ls -A "$overlay")" ]; then
    overlay_files="$(cd "$overlay" && find . -type f | sed 's|^\./||' | tr '\n' ' ')"
    echo "   overlay から追加: $overlay_files"
    cp -a "$overlay/." "$stage/"
fi

# app のときだけビルドする。db は sql を置くだけ
if [ "$role" = "app" ]; then
    # ビルドしてから反映するので、失敗しても動いているバイナリは壊れない
    cd "$stage/webapp/go"
    "$go" build -o "$bin" .
    cd /
fi

for p in $paths; do
    sub="${p#webapp/}"
    rsync -a --delete "$stage/$p/" "$root/$sub/"
done
printf '%s %s\n%s by %s\nrole: %s\noverlay: %s\n' "$short" "$subject" "$(date '+%F %T')" "$deployer" "$role" "${overlay_files:-なし}" > "$root/REVISION"

if [ "$role" = "db" ]; then
    if [ -n "$db_web_svc" ]; then
        sudo systemctl restart "$db_web_svc"
        echo "   OK: $root に sql を反映、$db_web_svc を再起動（初期化は POST /api/initialize で行う）"
    else
        echo "   OK: $root に sql を反映（初期化は POST /api/initialize で行う）"
    fi
    exit 0
fi

sudo systemctl restart "$app_svc"
sleep 1
if ! systemctl is-active --quiet "$app_svc"; then
    echo "!! $app_svc が起動していません" >&2
    sudo journalctl -u "$app_svc" -n 30 --no-pager >&2
    exit 1
fi
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$health" || true)"
if [ "$code" = "000" ] || [ "$code" -ge 500 ]; then
    echo "!! HTTP 応答が異常: $health -> $code" >&2
    sudo journalctl -u "$app_svc" -n 30 --no-pager >&2
    exit 1
fi
echo "   OK: $app_svc active, $health -> $code"
REMOTE
done

echo ""
if [ $build_only -eq 1 ]; then
    echo "==> 完了: ${targets[*]} で確認（反映はしていない）"
else
    echo "==> 完了: $short を ${targets[*]} へ"
fi
