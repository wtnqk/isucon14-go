package main

import (
	"net/http"
	"sync"
)

// matcher の呼び出しが重なっても、同じ椅子を二重に割り当てないようにする
var matchingMu sync.Mutex

type matchingChair struct {
	ID        string `db:"id"`
	Speed     int    `db:"speed"`
	Latitude  int    `db:"latitude"`
	Longitude int    `db:"longitude"`
}

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる
// 待っているライドを古い順にすべて取り出し、乗車位置への到着が一番早い空き椅子を割り当てる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	matchingMu.Lock()
	defer matchingMu.Unlock()

	rides := []Ride{}
	if err := db.SelectContext(ctx, &rides, `SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rides) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 空き椅子 = 受付中で、前のライドの状態遷移が6つとも記録されている椅子。
	// 椅子には前のライドが終わってから次を割り当てるので、未完了になり得るのは最後に割り当てたライド
	// （updated_at が最新のもの）だけ。それだけを調べる。
	// 最新の位置・ライドは MAX + GROUP BY で取る。相関サブクエリの ORDER BY ... LIMIT 1 だと
	// LIMIT が効かず、椅子ごとに履歴を全件読んでソートしてしまうため。
	// 位置が未送信の椅子は距離が分からないので、次回以降に回す
	rows := []matchingChair{}
	if err := db.SelectContext(ctx, &rows, `
SELECT chairs.id, chair_models.speed, chair_locations.latitude, chair_locations.longitude
FROM chairs
       INNER JOIN chair_models ON chair_models.name = chairs.model
       INNER JOIN (SELECT chair_id, MAX(created_at) AS created_at
                   FROM chair_locations
                   GROUP BY chair_id) latest_location ON latest_location.chair_id = chairs.id
       INNER JOIN chair_locations ON chair_locations.chair_id = latest_location.chair_id
                                 AND chair_locations.created_at = latest_location.created_at
       LEFT JOIN (SELECT chair_id, MAX(updated_at) AS updated_at
                  FROM rides
                  WHERE chair_id IS NOT NULL
                  GROUP BY chair_id) latest_ride ON latest_ride.chair_id = chairs.id
WHERE chairs.is_active = TRUE
  AND NOT EXISTS (SELECT 1
                  FROM rides
                  WHERE rides.chair_id = chairs.id
                    AND rides.updated_at = latest_ride.updated_at
                    AND (SELECT COUNT(cs.id) FROM ride_statuses cs WHERE cs.ride_id = rides.id) < 6)
`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 同じ時刻の位置が2件あると椅子が2行になり、二重に割り当ててしまうので1行にまとめる
	chairs := make([]matchingChair, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, chair := range rows {
		if !seen[chair.ID] {
			seen[chair.ID] = true
			chairs = append(chairs, chair)
		}
	}

	for _, ride := range rides {
		if len(chairs) == 0 {
			break
		}

		best := -1
		bestTime := 0.0
		for i, chair := range chairs {
			t := float64(calculateDistance(chair.Latitude, chair.Longitude, ride.PickupLatitude, ride.PickupLongitude)) / float64(chair.Speed)
			if best == -1 || t < bestTime {
				best, bestTime = i, t
			}
		}

		if _, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ? AND chair_id IS NULL", chairs[best].ID, ride.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// マッチしたので、ユーザー（椅子が決まったこと）と椅子（割り当てられたこと）に伝える
		publishRideChange(ride.UserID, chairs[best].ID)
		chairs = append(chairs[:best], chairs[best+1:]...)
	}

	w.WriteHeader(http.StatusNoContent)
}
