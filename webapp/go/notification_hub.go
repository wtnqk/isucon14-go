package main

// 通知エンドポイントを SSE で提供するための、プロセス内の pub/sub。
//
// 設計:
//   - 通知の「元ネタ」は ride_statuses に書かれた状態遷移そのもので、DB が唯一の真実。
//   - 状態遷移を書いたトランザクションがコミットした後で、そのライドの関係者（ユーザーと椅子）に
//     「このライドに変化があった」とだけ伝える。送る内容は購読者が DB から読み直して組み立てる。
//   - 購読者は「最後に通知したライドと、その時の状態」を覚えていて、そこから先を送る。
//     これで、接続前に起きた遷移も、接続中に起きた遷移も、発生順に一度以上は必ず送られる。
//   - ブロードキャストは「早く気づくため」だけの仕組みで、正しさは拠っていない。
//     送る内容は毎回 DB から読み直すので、publish を落としても通知は消えない（遅れるだけ）。
//     定期確認が最後の砦として、必ず状態の変化に追いつく。

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// notificationPollInterval ブロードキャストを取りこぼした場合の保険の確認間隔。
	// これは「通知が遅れる最悪の時間」でもあるので、仕様の3秒以内に収まる値を選ぶ
	notificationPollInterval = 300 * time.Millisecond
	// notificationHeartbeatInterval 接続維持のためのコメント行を送る間隔
	notificationHeartbeatInterval = 15 * time.Second
	// notificationBuildRetryInterval build がエラーになったときの再試行間隔。
	// DB が一時的に重いときに接続を切ってしまうと、再接続の嵐になるので、切らずに待つ
	notificationBuildRetryInterval = 100 * time.Millisecond
)

const (
	topicRideUser   = "ride:user:"
	topicRideChair  = "ride:chair:"
	topicUserNotify = "user:"
	topicChairState = "chair:"
)

// notificationHub はトピックごとの購読者を管理する
type notificationHub struct {
	mu     sync.Mutex
	topics map[string]map[*notificationSubscriber]struct{}
	// subscribers 現在の購読者数。SSE ではライド中ずっと接続を保持するので、これがそのまま
	// 「同時に張られている通知ストリームの数」になる
	subscribers atomic.Int64
	// publishes これまでに購読者を起こした回数
	publishes atomic.Int64
	// dropped 起こし損ねた回数（すでに起こし待ちだったため）
	dropped atomic.Int64
}

type notificationSubscriber struct {
	// wake 通知があると1つだけ値を入れるセマフォ
	wake chan struct{}
}

func newNotificationHub() *notificationHub {
	return &notificationHub{topics: make(map[string]map[*notificationSubscriber]struct{})}
}

func (h *notificationHub) subscribe(topics ...string) *notificationSubscriber {
	s := &notificationSubscriber{wake: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, topic := range topics {
		subs, ok := h.topics[topic]
		if !ok {
			subs = make(map[*notificationSubscriber]struct{})
			h.topics[topic] = subs
		}
		subs[s] = struct{}{}
	}
	h.subscribers.Add(1)
	return s
}

func (h *notificationHub) unsubscribe(s *notificationSubscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	found := false
	for topic, subs := range h.topics {
		if _, ok := subs[s]; ok {
			found = true
			delete(subs, s)
			if len(subs) == 0 {
				delete(h.topics, topic)
			}
		}
	}
	if found {
		// 二重に解除しても数え落とさないよう、実際に消せたときだけ減らす
		h.subscribers.Add(-1)
	}
}

// publish はトピックの購読者を起こす。キューが詰まっていても捨てるだけなので、呼び出し元はブロックしない
func (h *notificationHub) publish(topics ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, topic := range topics {
		for s := range h.topics[topic] {
			select {
			case s.wake <- struct{}{}:
				h.publishes.Add(1)
			default:
				// すでに起こし待ち。購読者は起きたときに DB から読み直すので、
				// 複数回の publish を1回の起床にまとめても取りこぼしにはならない
				h.dropped.Add(1)
			}
		}
	}
}

var hub = newNotificationHub()

// publishRideChange はライドの状態が変わったことを、そのライドの関係者に知らせる。
// トランザクションのコミット後に呼ぶこと
func publishRideChange(userID string, chairID string) {
	topics := []string{topicRideUser + userID, topicUserNotify + userID}
	if chairID != "" {
		topics = append(topics, topicRideChair+chairID, topicChairState+chairID)
	}
	hub.publish(topics...)
}

// publishChairActive は、椅子の受付状態が変わったことを知らせる
func publishChairActive(chairID string) {
	hub.publish(topicChairState + chairID)
}

// notificationDelivery は、通知を送る単位
type notificationDelivery struct {
	// rideKey 通知したライドのID。空なら記録しない
	rideKey string
	// lastStatus 通知した時点のライドの状態。次に「状態が変わったか」を判定するのに使う
	lastStatus string
	// body 送る JSON。nil なら送るものがない
	body []byte
}

// notificationCursor は購読者ごとの「最後に通知したライド」を持つ。
// 送信のたびに DB から読み直すので、接続を張り直しても続きから送れる。
type notificationCursor struct {
	// rideKey 最後に通知したライドのID。初期状態では空
	rideKey string
	// rideCreatedAt 最後に通知したライドの作成日時。それより後のライドを探すのに使う
	rideCreatedAt time.Time
	// lastStatus 最後に通知した時点のライドの状態
	lastStatus string
	// ready 接続直後の「最新を1つ送る」を済ませたかどうか
	ready bool
}

// streamNotifications は、build が返す通知を送り続ける。
// build は「前回送ったライドの続きから」組み立て、送るものがない場合は空の delivery を返す。
//
// 接続直後は必ず1回 build を呼んで最新の状態を送る（仕様: 接続したら即座に最新の状態を送る）。
// 以降はブロードキャストか保険の定期確認で build を呼ぶ。
func streamNotifications(
	ctx context.Context,
	w *sseWriter,
	sub *notificationSubscriber,
	build func(ctx context.Context, cur *notificationCursor) (notificationDelivery, error),
) error {
	cur := &notificationCursor{}
	flush := func() error {
		delivery, err := build(ctx, cur)
		if err != nil {
			// 組み立てに失敗した（DB が一時的に重いなど）。送ったことにせず、次に回す。
			// 接続は切らないので、ベンチマーカー側からは「通知が少し遅れた」だけに見える
			return errNotificationBuildFailed
		}
		if delivery.body == nil {
			return nil
		}
		if err := w.WriteData(delivery.body); err != nil {
			return err
		}
		if delivery.rideKey != "" {
			cur.rideKey = delivery.rideKey
			cur.lastStatus = delivery.lastStatus
			cur.rideCreatedAt = time.Time{}
			cur.ready = true
		}
		return w.Flush()
	}

	poll := time.NewTicker(notificationPollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(notificationHeartbeatInterval)
	defer heartbeat.Stop()
	retry := time.NewTicker(notificationBuildRetryInterval)
	defer retry.Stop()

	// run は flush して、エラーが「組み立ての一時的な失敗」なら握って続行する。
	// 接続が壊れた場合（WriteData のエラー）だけ、呼び出し元に返して終わらせる
	run := func() bool {
		err := flush()
		if err == nil {
			return true
		}
		if errors.Is(err, errNotificationBuildFailed) {
			// 次の retry で拾い直す
			return true
		}
		return false
	}

	if !run() {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sub.wake:
			if !run() {
				return nil
			}
		case <-poll.C:
			if !run() {
				return nil
			}
		case <-retry.C:
			if !run() {
				return nil
			}
		case <-heartbeat.C:
			if err := w.Heartbeat(); err != nil {
				return nil
			}
		}
	}
}

// errNotificationBuildFailed は、通知の組み立てに失敗したことを表す内部用のエラー。
// 接続は維持したまま次回に再試行する
var errNotificationBuildFailed = errors.New("notification build failed")
