package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// 通知を SSE で送るかどうか。false にすると、これまでのポーリング（JSON と retry_after_ms）に戻る
const notificationSSE = true

// SSE で、未送信の通知があるかを DB に確認する間隔（ライドが進んでいる間）
const notificationCheckInterval = 200 * time.Millisecond

// streamNotifications は SSE の接続を保ち、通知を送り続ける。
// app サーバーが複数台でも動くよう、アプリ内で通知を受け渡さず、接続ごとに DB を定期的に確認する。
//   - hasUnsent: 未送信の状態があるかを、軽いクエリで確認する。あわせて、しばらく通知が来ない状態
//     （ライドが終わって次を待っているなど）かを idle で返す
//   - build: 通知を1件作る。未送信の状態があれば古いものを1件送信済みにして consumed=true を返す。ライドが無ければ nil
//   - idleInterval: idle のときの確認の間隔。通知の遅れが判定に響かない状態だけ、確認を間引いて DB の負荷を下げる
func streamNotifications[T any](
	w http.ResponseWriter,
	r *http.Request,
	hasUnsent func(context.Context) (unsent bool, idle bool, err error),
	build func(context.Context) (data *T, consumed bool, err error),
	idleInterval time.Duration,
) {
	ctx := r.Context()
	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// nginx にレスポンスをためこませず、すぐにクライアントへ流させる
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	send := func(data *T) error {
		buf, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
			return err
		}
		return rc.Flush()
	}

	// 接続直後は、未送信がなくても最新の状態を送る（仕様で必須）
	first := true
	idle := false
	for {
		// 未送信の状態がある間は、待たずに古い順に続けて送る
		for {
			if !first {
				ok, isIdle, err := hasUnsent(ctx)
				idle = isIdle
				if err != nil {
					logStreamError(err)
					return
				}
				if !ok {
					break
				}
			}
			data, consumed, err := build(ctx)
			if err != nil {
				logStreamError(err)
				return
			}
			if data != nil && (first || consumed) {
				if err := send(data); err != nil {
					return
				}
			}
			first = false
			if !consumed {
				break
			}
		}

		wait := notificationCheckInterval
		if idle {
			wait = idleInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func logStreamError(err error) {
	if !errors.Is(err, context.Canceled) {
		slog.Error("notification stream", "err", err)
	}
}
