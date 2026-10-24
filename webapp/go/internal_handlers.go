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

	// 空き椅子 = 受付中で、割り当て済みのライドの通知（6状態）をすべて椅子に送り終えている椅子。
	// 椅子には前のライドが終わってから次を割り当てるので、未完了になり得るのは最後に割り当てたライド
	// （updated_at が最新のもの）だけ。それだけを調べる。
	// 最新の位置は chair_total_distances（座標送信のたびに更新）から、最新のライドは MAX + GROUP BY で取る。
	// 位置が未送信の椅子は距離が分からないので、次回以降に回す
	chairs := []matchingChair{}
	if err := db.SelectContext(ctx, &chairs, `
SELECT chairs.id,
       chair_models.speed,
       chair_total_distances.last_latitude  AS latitude,
       chair_total_distances.last_longitude AS longitude
FROM chairs
       INNER JOIN chair_models ON chair_models.name = chairs.model
       INNER JOIN chair_total_distances ON chair_total_distances.chair_id = chairs.id
       LEFT JOIN (SELECT chair_id, MAX(updated_at) AS updated_at
                  FROM rides
                  WHERE chair_id IS NOT NULL
                  GROUP BY chair_id) latest_ride ON latest_ride.chair_id = chairs.id
WHERE chairs.is_active = TRUE
  AND NOT EXISTS (SELECT 1
                  FROM rides
                  WHERE rides.chair_id = chairs.id
                    AND rides.updated_at = latest_ride.updated_at
                    AND (SELECT COUNT(chair_sent_at) FROM ride_statuses WHERE ride_id = rides.id) < 6)
`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
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
		chairs = append(chairs[:best], chairs[best+1:]...)
	}

	w.WriteHeader(http.StatusNoContent)
}
