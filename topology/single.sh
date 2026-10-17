# 初期状態に近い形: 各台が全部入り（nginx・アプリ・MySQL）。
# matcher は app1 だけ（app2・app3 の matcher は止める。各台の DB は別々なので、本来は害はない）。
HOSTS="app1 app2 app3"
ROLES_app1="nginx app worker db"
ROLES_app2="nginx app db"
ROLES_app3="nginx app db"
