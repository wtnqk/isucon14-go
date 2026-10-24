-- 初期データの rides と ride_statuses から chair_states を作る。
-- 今のライドは、椅子ごとに updated_at が最新のライド（これまでの判定と同じ）。
-- completed_sent は、これまでのマッチングの判定（6 つのステータスをすべて椅子に送り終えたか）に合わせる
INSERT INTO chair_states (chair_id, ride_id, ride_status, completed_sent)
SELECT rides.chair_id,
       rides.id,
       (SELECT status FROM ride_statuses WHERE ride_id = rides.id ORDER BY created_at DESC LIMIT 1),
       (SELECT COUNT(chair_sent_at) FROM ride_statuses WHERE ride_id = rides.id) = 6
FROM rides
       INNER JOIN (SELECT chair_id, MAX(updated_at) AS updated_at
                   FROM rides
                   WHERE chair_id IS NOT NULL
                   GROUP BY chair_id) latest_ride ON latest_ride.chair_id = rides.chair_id
                                                  AND latest_ride.updated_at = rides.updated_at;
