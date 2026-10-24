package main

// 通知を組み立てる処理。
//
// 仕様:
//   - 通知エンドポイントは「発生したすべての状態遷移を、発生した順序通りに、少なくとも1回以上」返す
//   - 接続したら即座に最新の状態を送る
//   - 状態が変わった時のみ送ることが望ましい
//
// 状態は ride_statuses に積まれ、rides の最新の状態が今の状態になる。
// 「どのライドを、どの状態まで通知したか」だけを購読者が覚えておき、送信のたびに DB から読み直す。
// 状態を書き込む側と送信側で受け渡しをする必要がないので、複数プロセスでも動く。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type notificationAudience int

const (
	notificationAudienceUser notificationAudience = iota
	notificationAudienceChair
)

// buildNotification は通知を1つ組み立てる。送るものがない場合は空の delivery を返す。
//
// 通知する単位は「状態遷移1つ」ではなく「ライド1つ」にしている。
// 状態遷移ごとに送ってしまうと、前の遷移を送った後に次の遷移が起きている場合に、
// 前の遷移の内容（前の状態）を今の DB から読み直せなくなってしまうため。
// ライド単位なら、通知するライドに変化があったかどうかだけを見ればよく、内容は常に最新でよい。
func buildNotification(ctx context.Context, audience notificationAudience, targetID string, cur *notificationCursor) (notificationDelivery, error) {
	var next *notificationRide
	var err error

	switch {
	case !cur.ready:
		// 接続直後。最新の状態を送る
		next, err = currentRide(ctx, audience, targetID)
	case cur.rideKey == "":
		// ライドがまだ無い。流すものはない
		return notificationDelivery{}, nil
	default:
		next, err = nextRide(ctx, audience, targetID, cur)
	}
	if err != nil {
		return notificationDelivery{}, err
	}
	if next == nil {
		return notificationDelivery{}, nil
	}

	body, err := buildRideNotificationJSON(ctx, next.ID, audience)
	if err != nil {
		return notificationDelivery{}, err
	}
	if body == nil {
		return notificationDelivery{}, nil
	}
	return notificationDelivery{rideKey: next.ID, lastStatus: next.LatestStatus, body: body}, nil
}

// notificationRide は通知の対象になるライド
type notificationRide struct {
	ID string
	// LatestStatus そのライドの最新の状態
	LatestStatus string
	// CreatedAt ライドの作成日時。前回通知したライドより後かどうかの判定に使う
	CreatedAt time.Time
}

const latestRideStatusOf = `(SELECT status FROM ride_statuses WHERE ride_id = rides.id ORDER BY created_at DESC, id DESC LIMIT 1)`

// currentRide は、対象の最新のライドを返す。ライドが無ければ nil
func currentRide(ctx context.Context, audience notificationAudience, targetID string) (*notificationRide, error) {
	var query string
	switch audience {
	case notificationAudienceUser:
		query = `
SELECT id, ` + latestRideStatusOf + ` AS latest_status
FROM rides
WHERE user_id = ?
ORDER BY created_at DESC
LIMIT 1`
	case notificationAudienceChair:
		query = `
SELECT id, ` + latestRideStatusOf + ` AS latest_status
FROM rides
WHERE chair_id = ?
ORDER BY updated_at DESC
LIMIT 1`
	}
	return selectRide(ctx, query, targetID)
}

// nextRide は、次に通知すべきライドを返す。
//
// 判定順:
//  1. 前回通知したライド（cur.rideKey）を引き直す（rideCreatedAt を知らない場合もここで埋まる）
//  2. そのライドの状態が cur.lastStatus から変わっていれば、それを送る
//  3. 変わっていなければ、それより後に作られたライドがあれば送る（ライドの履歴は created_at 順に並ぶ）
//
// 2 を先に見るのがポイント。`created_at > 前回のライド` を常に使うと、
// 前回のライドの変化があるときに、そちらを先送りして新しいライドの状態を先に送ってしまう。
func nextRide(ctx context.Context, audience notificationAudience, targetID string, cur *notificationCursor) (*notificationRide, error) {
	// 1. 前回通知したライドを引き直す
	sent, err := selectRide(ctx, `SELECT id, `+latestRideStatusOf+` AS latest_status, created_at FROM rides WHERE id = ?`, cur.rideKey)
	if err != nil {
		return nil, err
	}
	if sent == nil {
		// 通知したライドが消えている（初期化など）。改めて最新から取り直す
		cur.rideKey = ""
		cur.ready = false
		return nil, nil
	}
	cur.rideCreatedAt = sent.CreatedAt

	// 2. 前回のライドの状態が変わっていれば、それを送る
	if sent.LatestStatus != cur.lastStatus {
		return sent, nil
	}

	// 3. 前回のライドより後に作られたライドを探す
	var byCreated string
	switch audience {
	case notificationAudienceUser:
		byCreated = `SELECT id, ` + latestRideStatusOf + ` AS latest_status
FROM rides
WHERE user_id = ?
  AND created_at > ?
ORDER BY created_at
LIMIT 1`
	case notificationAudienceChair:
		byCreated = `SELECT id, ` + latestRideStatusOf + ` AS latest_status
FROM rides
WHERE chair_id = ?
  AND created_at > ?
ORDER BY created_at
LIMIT 1`
	}
	return selectRide(ctx, byCreated, targetID, cur.rideCreatedAt)
}

// selectRide は rides を1行だけ引く
func selectRide(ctx context.Context, query string, args ...interface{}) (*notificationRide, error) {
	typed := []struct {
		ID           string    `db:"id"`
		LatestStatus string    `db:"latest_status"`
		CreatedAt    time.Time `db:"created_at"`
	}{}
	if err := db.SelectContext(ctx, &typed, query, args...); err != nil {
		return nil, err
	}
	if len(typed) == 0 {
		return nil, nil
	}
	return &notificationRide{ID: typed[0].ID, LatestStatus: typed[0].LatestStatus, CreatedAt: typed[0].CreatedAt}, nil
}

// retryAfterMsForSSE SSE ではポーリングしないが、JSON クライアントとの互換のために値を入れておく
const retryAfterMsForSSE = 300

// buildRideNotificationJSON は、ライドの今の状態を {data: ..., retry_after_ms: ...} の形で組み立てる
func buildRideNotificationJSON(ctx context.Context, rideID string, audience notificationAudience) ([]byte, error) {
	ride := &Ride{}
	if err := db.GetContext(ctx, ride, `SELECT * FROM rides WHERE id = ?`, rideID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	status, err := getLatestRideStatus(ctx, db, rideID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	if audience == notificationAudienceChair {
		user := &User{}
		if err := db.GetContext(ctx, user, `SELECT * FROM users WHERE id = ?`, ride.UserID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}

		return json.Marshal(&chairGetNotificationResponse{
			Data: &chairGetNotificationResponseData{
				RideID: ride.ID,
				User: simpleUser{
					ID:   user.ID,
					Name: user.Firstname + " " + user.Lastname,
				},
				PickupCoordinate: Coordinate{
					Latitude:  ride.PickupLatitude,
					Longitude: ride.PickupLongitude,
				},
				DestinationCoordinate: Coordinate{
					Latitude:  ride.DestinationLatitude,
					Longitude: ride.DestinationLongitude,
				},
				Status: status,
			},
			RetryAfterMs: retryAfterMsForSSE,
		})
	}

	fare, err := calculateDiscountedFare(ctx, db, ride.UserID, ride, ride.PickupLatitude, ride.PickupLongitude, ride.DestinationLatitude, ride.DestinationLongitude)
	if err != nil {
		return nil, err
	}

	response := &appGetNotificationResponse{
		Data: &appGetNotificationResponseData{
			RideID: ride.ID,
			PickupCoordinate: Coordinate{
				Latitude:  ride.PickupLatitude,
				Longitude: ride.PickupLongitude,
			},
			DestinationCoordinate: Coordinate{
				Latitude:  ride.DestinationLatitude,
				Longitude: ride.DestinationLongitude,
			},
			Fare:      fare,
			Status:    status,
			CreatedAt: ride.CreatedAt.UnixMilli(),
			UpdateAt:  ride.UpdatedAt.UnixMilli(),
		},
		RetryAfterMs: retryAfterMsForSSE,
	}

	if ride.ChairID.Valid {
		chair := &Chair{}
		if err := db.GetContext(ctx, chair, `SELECT * FROM chairs WHERE id = ?`, ride.ChairID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return json.Marshal(response)
			}
			return nil, err
		}

		stats, err := getChairStats(ctx, db, chair.ID)
		if err != nil {
			return nil, err
		}

		response.Data.Chair = &appGetNotificationResponseChair{
			ID:    chair.ID,
			Name:  chair.Name,
			Model: chair.Model,
			Stats: stats,
		}
	}

	return json.Marshal(response)
}
