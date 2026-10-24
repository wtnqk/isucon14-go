package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var erroredUpstream = errors.New("errored upstream")

type paymentGatewayPostPaymentRequest struct {
	Amount int `json:"amount"`
}

// 決済の再送の上限と間隔。決済サーバーは混んでいると一定の確率で失敗するが、
// 失敗するかは毎回ランダムなので、待っても成功しやすくはならない。短い間隔ですぐ送り直す
const (
	paymentMaxAttempts   = 10
	paymentRetryInterval = 10 * time.Millisecond
)

// requestPaymentGatewayPostPayment は決済を依頼し、成功するまで送り直す。
// idempotencyKey（ライド ID）を Idempotency-Key ヘッダで送るので、失敗の応答が返っても
// 実は決済済みだった場合に、送り直しで二重に支払われることはない。
// そのため、失敗のたびに決済一覧を取得して成否を確かめる必要はない
func requestPaymentGatewayPostPayment(ctx context.Context, paymentGatewayURL string, token string, idempotencyKey string, param *paymentGatewayPostPaymentRequest) error {
	b, err := json.Marshal(param)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt < paymentMaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(paymentRetryInterval)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, paymentGatewayURL+"/payments", bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", idempotencyKey)

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()

		switch {
		case res.StatusCode == http.StatusNoContent:
			return nil
		case res.StatusCode == http.StatusConflict || res.StatusCode >= 500:
			// 409 は同じキーの決済が処理中。5xx は混雑による失敗。どちらも送り直す
			lastErr = fmt.Errorf("[POST /payments] unexpected status code (%d)", res.StatusCode)
		default:
			// 400 や 422 は送り直しても成功しない
			return fmt.Errorf("[POST /payments] unexpected status code (%d)", res.StatusCode)
		}
	}

	return fmt.Errorf("%w: %v", erroredUpstream, lastErr)
}
