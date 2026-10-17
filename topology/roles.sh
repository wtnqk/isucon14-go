# 役割とサービスの対応（ISUCON14）。topology.sh がこのディレクトリの構成ファイルより先に読む。
# topology.sh は isucon-cheatsheet の scripts/topology.sh。
#   調べ方: systemctl list-unit-files --type=service | grep -i isu

# 使う役割の一覧
ROLE_NAMES="nginx app worker db"

# 役割ごとに動かすサービス
ROLE_SERVICES_nginx="nginx.service"
ROLE_SERVICES_app="isuride-go.service isuride-payment_mock.service"
# ISUCON14 の matcher は https://isuride.xiv.isucon.net（/etc/hosts で 127.0.0.1）を叩くので、
# nginx と app がある台に置く
ROLE_SERVICES_worker="isuride-matcher.service"
ROLE_SERVICES_db="mysql.service"

# 1台にしか置いてはいけない役割。2台以上に書くと topology.sh が止める
# （ISUCON14 は matcher が2台で動くと、同じライドを二重にマッチングして FAIL する）
SINGLETON_ROLES="worker"

DB_ROLE="db"     # DB の役割
APP_ROLE="app"   # DB に接続する役割。この台の APP_ENV_FILE の APP_DB_HOST_KEY を書き換える

# アプリが読む環境変数ファイル（systemd の EnvironmentFile）と、その中の DB 接続先の変数名
APP_ENV_FILE="/home/isucon/env.sh"
APP_DB_HOST_KEY="ISUCON_DB_HOST"
DB_PORT=3306

# アプリの台で、DB にログインできるか試すコマンド（空なら TCP の疎通だけ見る）
DB_CHECK_CMD='. /home/isucon/env.sh && mysql -h"$ISUCON_DB_HOST" -P"$ISUCON_DB_PORT" -u"$ISUCON_DB_USER" -p"$ISUCON_DB_PASSWORD" -N -e "select 1" "$ISUCON_DB_NAME"'

# アプリの台で叩いて 4xx が返れば生きている、という URL（副作用のないもの）
HEALTH_URL="http://127.0.0.1:8080/api/app/notification"

# "-" を含まない台の名前に付ける接頭辞（例: isucon14- なら app1 -> isucon14-app1）
DEPLOY_HOST_PREFIX="isucon14-"
