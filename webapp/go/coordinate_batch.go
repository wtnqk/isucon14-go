package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/oklog/ulid/v2"
)

// 座標送信は 1 秒に約 1,000 件あり、1 件ごとに DB へ書くと DB の負荷の大半を占めていた。
// ベンチの椅子は返事を待ってから次に動くので、返事が遅いと椅子が理想より遅く動く。
// そこで座標送信は受け取るだけですぐに返し、DB への書き込みは coordinateFlushInterval ごとにまとめて行う。
// 移動距離と最新の座標の反映は仕様で 3 秒の猶予があり、乗車地点・目的地への到着（PICKUP・ARRIVED）の記録も
// この間隔の分だけ遅れるが、それぞれの判定の猶予に収まる。
//
// app サーバーが複数台になる場合は、同じ椅子の座標が同じ台に届くよう振り分ける必要がある
// （別々の台でまとめて書くと、順番が前後して移動距離がずれるため）。
const coordinateFlushInterval = 100 * time.Millisecond

type pendingCoordinate struct {
	ChairID    string
	Latitude   int
	Longitude  int
	RecordedAt time.Time
}

var (
	pendingCoordinatesMu sync.Mutex
	pendingCoordinates   []pendingCoordinate
	// 書き込みの途中で初期化が走らないよう、書き込みと初期化を排他にする
	coordinateFlushMu sync.Mutex
)

// 書き込みが追いついているかを確かめるため、5 秒ごとにまとめてログに出す
const coordinateStatsInterval = 5 * time.Second

type coordinateFlushStats struct {
	flushes     int
	points      int
	arrivals    int
	maxBatch    int
	totalTime   time.Duration
	maxTime     time.Duration
	maxLag      time.Duration // 書き込んだ座標のうち、受け取ってから書き終わるまでに一番長くかかったもの
	errors      int
	lastLogTime time.Time
}

var (
	coordinateStatsMu sync.Mutex
	coordinateStats   coordinateFlushStats
)

func recordCoordinateFlush(batch []pendingCoordinate, arrivals int, elapsed time.Duration, err error) {
	now := time.Now()
	coordinateStatsMu.Lock()
	defer coordinateStatsMu.Unlock()
	st := &coordinateStats
	if st.lastLogTime.IsZero() {
		st.lastLogTime = now
	}
	st.flushes++
	if err != nil {
		st.errors++
	} else {
		st.points += len(batch)
		st.arrivals += arrivals
		if len(batch) > 0 {
			st.maxLag = max(st.maxLag, now.Sub(batch[0].RecordedAt))
		}
	}
	st.maxBatch = max(st.maxBatch, len(batch))
	st.totalTime += elapsed
	st.maxTime = max(st.maxTime, elapsed)
	if now.Sub(st.lastLogTime) < coordinateStatsInterval {
		return
	}
	pendingCoordinatesMu.Lock()
	pending := len(pendingCoordinates)
	pendingCoordinatesMu.Unlock()
	slog.Info("coordinate flush",
		"flushes", st.flushes,
		"points", st.points,
		"arrivals", st.arrivals,
		"max_batch", st.maxBatch,
		"avg_ms", st.totalTime.Milliseconds()/int64(st.flushes),
		"max_ms", st.maxTime.Milliseconds(),
		"max_lag_ms", st.maxLag.Milliseconds(),
		"pending", pending,
		"errors", st.errors)
	coordinateStats = coordinateFlushStats{lastLogTime: now}
}

func enqueueCoordinate(c pendingCoordinate) {
	pendingCoordinatesMu.Lock()
	pendingCoordinates = append(pendingCoordinates, c)
	pendingCoordinatesMu.Unlock()
}

func startCoordinateFlusher() {
	go func() {
		ticker := time.NewTicker(coordinateFlushInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := flushCoordinates(context.Background()); err != nil {
				slog.Error("flush coordinates", "err", err)
			}
		}
	}()
}

// withCoordinatesReset は、たまっている座標を捨ててから fn を実行する。fn の間は書き込みを止める。
// POST /api/initialize でデータを作り直すときに使う（作り直す前の座標を、作り直した後に書かないため）
func withCoordinatesReset(fn func() error) error {
	coordinateFlushMu.Lock()
	defer coordinateFlushMu.Unlock()
	pendingCoordinatesMu.Lock()
	pendingCoordinates = nil
	pendingCoordinatesMu.Unlock()
	return fn()
}

func flushCoordinates(ctx context.Context) error {
	coordinateFlushMu.Lock()
	defer coordinateFlushMu.Unlock()

	pendingCoordinatesMu.Lock()
	batch := pendingCoordinates
	pendingCoordinates = nil
	pendingCoordinatesMu.Unlock()
	if len(batch) == 0 {
		return nil
	}

	start := time.Now()
	arrivals, err := writeCoordinates(ctx, batch)
	recordCoordinateFlush(batch, arrivals, time.Since(start), err)
	if err != nil {
		// 書けなかった分は、後から届いた分より前に戻して次回に回す（椅子ごとの順番を保つため）
		pendingCoordinatesMu.Lock()
		pendingCoordinates = append(batch, pendingCoordinates...)
		pendingCoordinatesMu.Unlock()
		return err
	}
	return nil
}

// writeCoordinates は、届いた座標を 1 つのトランザクションで書き込み、記録した到着の件数を返す。
// 件数にかかわらず文の数は一定にする（1 回が遅くなると次の回に溜まる件数が増え、さらに遅くなるため）。
//   - 移動距離の合計：前回の座標から、届いた順に座標の差を足す。最新の座標と、最後に受け取った時刻も更新する
//   - 到着の判定：乗車地点（ENROUTE のとき）・目的地（CARRYING のとき）に着いたら PICKUP・ARRIVED を記録する
func writeCoordinates(ctx context.Context, batch []pendingCoordinate) (int, error) {
	// 椅子ごとに、届いた順に並べる
	chairIDs := []string{}
	byChair := map[string][]pendingCoordinate{}
	for _, c := range batch {
		if _, ok := byChair[c.ChairID]; !ok {
			chairIDs = append(chairIDs, c.ChairID)
		}
		byChair[c.ChairID] = append(byChair[c.ChairID], c)
	}

	tx, err := db.Beginx()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// 前回の座標と移動距離の合計
	type distanceRow struct {
		ChairID       string `db:"chair_id"`
		TotalDistance int    `db:"total_distance"`
		LastLatitude  int    `db:"last_latitude"`
		LastLongitude int    `db:"last_longitude"`
	}
	query, args, err := sqlx.In(`SELECT chair_id, total_distance, last_latitude, last_longitude FROM chair_total_distances WHERE chair_id IN (?) FOR UPDATE`, chairIDs)
	if err != nil {
		return 0, err
	}
	distances := []distanceRow{}
	if err := tx.SelectContext(ctx, &distances, query, args...); err != nil {
		return 0, err
	}
	prevByChair := map[string]distanceRow{}
	for _, d := range distances {
		prevByChair[d.ChairID] = d
	}

	// 椅子の今のライドと、その最新ステータス
	type rideRow struct {
		ChairID              string `db:"chair_id"`
		RideStatus           string `db:"ride_status"`
		RideID               string `db:"ride_id"`
		PickupLatitude       int    `db:"pickup_latitude"`
		PickupLongitude      int    `db:"pickup_longitude"`
		DestinationLatitude  int    `db:"destination_latitude"`
		DestinationLongitude int    `db:"destination_longitude"`
	}
	query, args, err = sqlx.In(`
SELECT chair_states.chair_id,
       chair_states.ride_status,
       rides.id AS ride_id,
       rides.pickup_latitude,
       rides.pickup_longitude,
       rides.destination_latitude,
       rides.destination_longitude
FROM chair_states
       INNER JOIN rides ON rides.id = chair_states.ride_id
WHERE chair_states.chair_id IN (?)`, chairIDs)
	if err != nil {
		return 0, err
	}
	rides := []rideRow{}
	if err := tx.SelectContext(ctx, &rides, query, args...); err != nil {
		return 0, err
	}
	rideByChair := map[string]rideRow{}
	for _, r := range rides {
		rideByChair[r.ChairID] = r
	}

	type arrival struct {
		ChairID string
		RideID  string
		Status  string
	}
	arrivals := []arrival{}
	distanceArgs := make([]any, 0, len(chairIDs)*5)
	for _, chairID := range chairIDs {
		prev, hasPrev := prevByChair[chairID]
		total, lastLat, lastLon := prev.TotalDistance, prev.LastLatitude, prev.LastLongitude
		ride, hasRide := rideByChair[chairID]
		status := ride.RideStatus

		points := byChair[chairID]
		for _, p := range points {
			if hasPrev {
				total += abs(p.Latitude-lastLat) + abs(p.Longitude-lastLon)
			}
			lastLat, lastLon, hasPrev = p.Latitude, p.Longitude, true

			if !hasRide {
				continue
			}
			switch {
			case status == "ENROUTE" && p.Latitude == ride.PickupLatitude && p.Longitude == ride.PickupLongitude:
				status = "PICKUP"
				arrivals = append(arrivals, arrival{ChairID: chairID, RideID: ride.RideID, Status: status})
			case status == "CARRYING" && p.Latitude == ride.DestinationLatitude && p.Longitude == ride.DestinationLongitude:
				status = "ARRIVED"
				arrivals = append(arrivals, arrival{ChairID: chairID, RideID: ride.RideID, Status: status})
			}
		}
		last := points[len(points)-1]
		distanceArgs = append(distanceArgs, chairID, total, last.RecordedAt, lastLat, lastLon)
	}

	// recorded_at と total_distance_updated_at は同じ時刻にそろえる（ベンチがこの2つを突き合わせる）
	if _, err := tx.ExecContext(ctx, `
INSERT INTO chair_total_distances (chair_id, total_distance, total_distance_updated_at, last_latitude, last_longitude)
VALUES (?, ?, ?, ?, ?)`+strings.Repeat(", (?, ?, ?, ?, ?)", len(chairIDs)-1)+`
ON DUPLICATE KEY UPDATE total_distance            = VALUES(total_distance),
                        total_distance_updated_at = VALUES(total_distance_updated_at),
                        last_latitude             = VALUES(last_latitude),
                        last_longitude            = VALUES(last_longitude)`,
		distanceArgs...); err != nil {
		return 0, err
	}

	if len(arrivals) > 0 {
		statusArgs := make([]any, 0, len(arrivals)*3)
		caseArgs := make([]any, 0, len(arrivals)*2)
		inArgs := make([]any, 0, len(arrivals)*2)
		for _, a := range arrivals {
			statusArgs = append(statusArgs, ulid.Make().String(), a.RideID, a.Status)
			caseArgs = append(caseArgs, a.ChairID, a.Status)
			inArgs = append(inArgs, a.ChairID, a.RideID)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO ride_statuses (id, ride_id, status) VALUES (?, ?, ?)"+strings.Repeat(", (?, ?, ?)", len(arrivals)-1),
			statusArgs...); err != nil {
			return 0, err
		}
		// updateChairRideStatus と同じく、今のライドが到着したライドのときだけ更新する
		if _, err := tx.ExecContext(ctx,
			"UPDATE chair_states SET ride_status = CASE chair_id"+strings.Repeat(" WHEN ? THEN ?", len(arrivals))+
				" END WHERE (chair_id, ride_id) IN ((?, ?)"+strings.Repeat(", (?, ?)", len(arrivals)-1)+")",
			append(caseArgs, inArgs...)...); err != nil {
			return 0, err
		}
	}

	return len(arrivals), tx.Commit()
}
