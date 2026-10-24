package main

import (
	"context"
	"net/http"
	"strings"
	"sync"

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

type matchingAssignment struct {
	RideID  string
	ChairID string
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

	// 割り当てる組み合わせを先にすべて決める
	assignments := []matchingAssignment{}
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

		assignments = append(assignments, matchingAssignment{RideID: ride.ID, ChairID: chairs[best].ID})
		chairs = append(chairs[:best], chairs[best+1:]...)
	}

	if err := assignRides(ctx, assignments); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// assignRides は、ライドへの椅子の割り当てと、椅子の今のライド（chair_states）の更新を、
// 件数にかかわらず 1 つのトランザクションの 2 文で行う（1 件ずつ書くと、件数に比例して DB との往復が増えるため）。
// 割り当て済みのライドが混ざっていて更新件数が合わなければ、ロールバックして 1 件ずつ処理し直す
// （割り当て済みのライドは飛ばす）
func assignRides(ctx context.Context, assignments []matchingAssignment) error {
	if len(assignments) == 0 {
		return nil
	}

	ok, err := assignRidesBulk(ctx, assignments)
	if err != nil || ok {
		return err
	}

	for _, a := range assignments {
		if _, err := assignRidesBulk(ctx, []matchingAssignment{a}); err != nil {
			return err
		}
	}
	return nil
}

// assignRidesBulk は assignments をまとめて書き込む。
// いずれかのライドが既に割り当て済みだった（更新件数が合わない）場合は、何も書かずに false を返す
func assignRidesBulk(ctx context.Context, assignments []matchingAssignment) (bool, error) {
	tx, err := db.Beginx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// UPDATE rides SET chair_id = CASE id WHEN ? THEN ? ... END WHERE id IN (?, ...) AND chair_id IS NULL
	caseArgs := make([]any, 0, len(assignments)*2)
	inArgs := make([]any, 0, len(assignments))
	for _, a := range assignments {
		caseArgs = append(caseArgs, a.RideID, a.ChairID)
		inArgs = append(inArgs, a.RideID)
	}
	query := "UPDATE rides SET chair_id = CASE id" + strings.Repeat(" WHEN ? THEN ?", len(assignments)) +
		" END WHERE id IN (?" + strings.Repeat(", ?", len(assignments)-1) + ") AND chair_id IS NULL"
	result, err := tx.ExecContext(ctx, query, append(caseArgs, inArgs...)...)
	if err != nil {
		return false, err
	}
	if n, err := result.RowsAffected(); err != nil {
		return false, err
	} else if n != int64(len(assignments)) {
		return false, nil
	}

	// 割り当てた時点のライドの状態は MATCHING（ライド作成時に記録される）
	stateArgs := make([]any, 0, len(assignments)*2)
	for _, a := range assignments {
		stateArgs = append(stateArgs, a.ChairID, a.RideID)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO chair_states (chair_id, ride_id, ride_status, completed_sent)
VALUES `+"(?, ?, 'MATCHING', FALSE)"+strings.Repeat(", (?, ?, 'MATCHING', FALSE)", len(assignments)-1)+`
ON DUPLICATE KEY UPDATE ride_id        = VALUES(ride_id),
                        ride_status    = VALUES(ride_status),
                        completed_sent = VALUES(completed_sent)`,
		stateArgs...); err != nil {
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
