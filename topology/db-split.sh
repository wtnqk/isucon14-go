# DB を app2 に分ける（ISUCON14 の上位チームの形）。
#   app1: nginx・アプリ・matcher（ベンチの向き先）
#   app2: MySQL だけ
#   app3: 使わない（何も動かさない）
HOSTS="app1 app2 app3"
ROLES_app1="nginx app worker"
ROLES_app2="db"
ROLES_app3=""
