package main

import (
	"context"
	"net/http"
)

// maxMatchPerRequest は1回のマッチング呼び出しで割り当てるライドの上限。
// matcher は短い間隔で叩かれるので、空き椅子が複数戻ったときに取りこぼさないようまとめて割り当てる。
const maxMatchPerRequest = 20

// internalGetMatching はインスタンス内から一定間隔で叩かせ、椅子とライドをマッチングさせる。
//
// マッチングの指針:
//   - 待たせているライドを古い順に優先する
//   - ベンチの評価は「割り当てられた椅子が乗車地点から 10*速度 以内にいたか(Dispatch)」と
//     「乗車地点までの所要時間が理想に近いか(Pickup)」なので、
//     まず 10*速度 以内に収まる椅子を優先し、その中で見積もり時間が短いものを選ぶ
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rides, err := findWaitingRides(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(rides) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 空き椅子を1回だけ取り、ライドごとに使い捨てていく。
	// 割り当てに成功した椅子は候補から外すので、同じ椅子が二重に割り当たらない。
	chairs, err := listIdleChairs(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(chairs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	used := make([]bool, len(chairs))
	matched := 0
	for _, ride := range rides {
		best := -1
		bestKey := 0
		for i, chair := range chairs {
			if used[i] {
				continue
			}
			distance := abs(chair.Latitude-ride.PickupLatitude) + abs(chair.Longitude-ride.PickupLongitude)
			// Dispatch の条件(10*速度以内)を満たす椅子を第一優先にし、
			// 同条件内では所要時間の見積もり(distance/speed)が短い順に選ぶ。
			// 速度は小さいほど遅いので、逆数で比較する。
			key := distance * 1000 / chair.Speed
			if distance >= 10*chair.Speed {
				// 条件を満たさない椅子は大きな値にして後回しにする
				key += 1 << 30
			}
			if best == -1 || key < bestKey {
				best = i
				bestKey = key
			}
		}
		if best == -1 {
			break
		}

		// 別のマッチング処理が先に割り当てていた場合は上書きしない
		result, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ? WHERE id = ? AND chair_id IS NULL", chairs[best].ID, ride.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		affected, err := result.RowsAffected()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if affected == 0 {
			continue
		}
		used[best] = true
		matched++
		if matched >= maxMatchPerRequest {
			break
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

type waitingRide struct {
	ID              string `db:"id"`
	PickupLatitude  int    `db:"pickup_latitude"`
	PickupLongitude int    `db:"pickup_longitude"`
}

// findWaitingRides は割り当て待ちのライドを古い順に返す。
func findWaitingRides(ctx context.Context) ([]waitingRide, error) {
	rides := []waitingRide{}
	if err := db.SelectContext(
		ctx,
		&rides,
		`SELECT id, pickup_latitude, pickup_longitude
		   FROM rides
		  WHERE chair_id IS NULL
		  ORDER BY created_at
		  LIMIT ?`,
		maxMatchPerRequest,
	); err != nil {
		return nil, err
	}
	return rides, nil
}

type idleChair struct {
	ID        string `db:"id"`
	Latitude  int    `db:"latitude"`
	Longitude int    `db:"longitude"`
	Speed     int    `db:"speed"`
}

// listIdleChairs は割り当て可能な椅子を最新位置・速度つきで返す。
// 走行中のライドを持つ椅子は除外する。
func listIdleChairs(ctx context.Context) ([]idleChair, error) {
	// is_active で先に絞ってから位置と走行状態を引く。先に椅子を絞ることで、
	// 各サブクエリが idx_chair_id_created_at / idx_ride_id_created_at の Index lookup になる。
	chairs := []idleChair{}
	if err := db.SelectContext(
		ctx,
		&chairs,
		`SELECT c.id, cl.latitude, cl.longitude, cm.speed
		   FROM (SELECT id, model FROM chairs WHERE is_active = TRUE) c
		   JOIN chair_models cm ON cm.name = c.model
		   JOIN chair_locations cl
		     ON cl.id = (SELECT cl2.id FROM chair_locations cl2 WHERE cl2.chair_id = c.id ORDER BY cl2.created_at DESC LIMIT 1)
		  WHERE NOT EXISTS (
		          SELECT 1
		            FROM rides r
		           WHERE r.chair_id = c.id
		             AND (SELECT rs.status FROM ride_statuses rs WHERE rs.ride_id = r.id ORDER BY rs.created_at DESC LIMIT 1) <> 'COMPLETED'
		        )`,
	); err != nil {
		return nil, err
	}
	return chairs, nil
}
