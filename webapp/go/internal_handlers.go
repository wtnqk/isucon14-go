package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

// internalGetMatching はインスタンス内から一定間隔で叩かせ、椅子とライドをマッチングさせる。
//
// マッチングの指針:
//   - 最も待たせているライドを優先する
//   - 乗車地点から近く、速い椅子を優先する。ベンチは「割り当てられた椅子が乗車地点から
//     10*速度 以内にいたか」と「乗車地点までの所要時間が理想に近いか」を評価するため、
//     マンハッタン距離を速度で割った見積もり時間で並べる
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	ride := &Ride{}
	if err := db.GetContext(ctx, ride, `SELECT * FROM rides WHERE chair_id IS NULL ORDER BY created_at LIMIT 1`); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	chairID, err := findMatchableChair(ctx, ride.PickupLatitude, ride.PickupLongitude)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 空いている椅子が無い
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// 別のマッチング処理が先に割り当てていた場合は上書きしない
	result, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ? AND chair_id IS NULL", chairID, ride.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if affected, err := result.RowsAffected(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	} else if affected == 0 {
		// 割り当てに負けたので、この椅子は次回以降に回す
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// findMatchableChair は乗車地点に近い空き椅子を1台返す。
// 空き椅子が無い場合は sql.ErrNoRows を返す。
func findMatchableChair(ctx context.Context, pickupLatitude, pickupLongitude int) (string, error) {
	// is_active で先に絞ってから位置と走行状態を引く。先に椅子を絞ることで、
	// 各サブクエリが idx_chair_id_created_at / idx_ride_id_created_at の Index lookup になる。
	//
	// 並び順は「乗車地点までの見積もり所要時間(distance/speed)」を最優先にする。
	// ベンチは「割り当てられた椅子が乗車地点から 10*速度 以内にいたか」と
	// 「乗車地点までの所要時間が理想に近いか」を評価するため、速い椅子は遠くても有利になる。
	const query = `
SELECT c.id,
       ABS(cl.latitude - ?) + ABS(cl.longitude - ?) AS distance
  FROM (SELECT id, model FROM chairs WHERE is_active = TRUE) c
  JOIN chair_models cm ON cm.name = c.model
  JOIN chair_locations cl
    ON cl.id = (SELECT cl2.id FROM chair_locations cl2 WHERE cl2.chair_id = c.id ORDER BY cl2.created_at DESC LIMIT 1)
 WHERE NOT EXISTS (
         SELECT 1
           FROM rides r
          WHERE r.chair_id = c.id
            AND (SELECT rs.status FROM ride_statuses rs WHERE rs.ride_id = r.id ORDER BY rs.created_at DESC LIMIT 1) <> 'COMPLETED'
       )
 ORDER BY distance / cm.speed, distance, c.id
 LIMIT 1`

	var row struct {
		ID string `db:"id"`
	}
	if err := db.GetContext(ctx, &row, query, pickupLatitude, pickupLongitude); err != nil {
		return "", err
	}
	return row.ID, nil
}
