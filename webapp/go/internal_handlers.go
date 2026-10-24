package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
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
	start := time.Now()

	rides := []Ride{}
	if err := db.SelectContext(ctx, &rides, `SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rides) == 0 {
		slog.Info("matching", "waiting", 0, "elapsed_ms", time.Since(start).Milliseconds())
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 空き椅子 = 受付中で、今のライドがない（一度も割り当てられていない）か、
	// 今のライドの COMPLETED を椅子に通知し終えている椅子（chair_states を参照）。
	// 最新の位置は chair_total_distances（座標送信のたびに更新）から取る。
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
       LEFT JOIN chair_states ON chair_states.chair_id = chairs.id
WHERE chairs.is_active = TRUE
  AND (chair_states.chair_id IS NULL OR chair_states.completed_sent = TRUE)
`); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 調査用: 1 回のマッチングで何件割り当てたかをログに出す
	candidates, assigned, skipped := len(chairs), 0, 0
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

		ok, err := assignRide(r, ride.ID, chairs[best].ID)
		if err != nil {
			slog.Info("matching", "waiting", len(rides), "candidates", candidates, "assigned", assigned, "skipped", skipped, "elapsed_ms", time.Since(start).Milliseconds(), "err", err)
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if ok {
			assigned++
		} else {
			skipped++
		}
		chairs = append(chairs[:best], chairs[best+1:]...)
	}
	slog.Info("matching", "waiting", len(rides), "candidates", candidates, "assigned", assigned, "skipped", skipped, "elapsed_ms", time.Since(start).Milliseconds())

	w.WriteHeader(http.StatusNoContent)
}

// assignRide はライドに椅子を割り当て、椅子の今のライド（chair_states）を更新する。
// ライドが既に割り当て済みなら何もせず false を返す
func assignRide(r *http.Request, rideID, chairID string) (bool, error) {
	ctx := r.Context()
	tx, err := db.Beginx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ? AND chair_id IS NULL", chairID, rideID)
	if err != nil {
		return false, err
	}
	if n, err := result.RowsAffected(); err != nil {
		return false, err
	} else if n == 0 {
		return false, nil
	}

	// 割り当てた時点のライドの状態は MATCHING（ライド作成時に記録される）
	if _, err := tx.ExecContext(ctx, `
INSERT INTO chair_states (chair_id, ride_id, ride_status, completed_sent)
VALUES (?, ?, 'MATCHING', FALSE)
ON DUPLICATE KEY UPDATE ride_id        = VALUES(ride_id),
                        ride_status    = VALUES(ride_status),
                        completed_sent = VALUES(completed_sent)`,
		chairID, rideID); err != nil {
		return false, err
	}

	return true, tx.Commit()
}

// updateChairRideStatus は、椅子に割り当て済みのライドのステータスを記録したときに、
// 同じトランザクションで椅子の今のライドの状態（chair_states.ride_status）も更新する
func updateChairRideStatus(ctx context.Context, tx *sqlx.Tx, chairID, rideID, status string) error {
	_, err := tx.ExecContext(ctx, "UPDATE chair_states SET ride_status = ? WHERE chair_id = ? AND ride_id = ?", status, chairID, rideID)
	return err
}
