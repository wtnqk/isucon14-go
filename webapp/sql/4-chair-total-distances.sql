-- 初期データの chair_locations から chair_total_distances を作る。
-- 集計は元の GET /api/owner/chairs のクエリと同じ（座標の差のマンハッタン距離を created_at 順に足す）
INSERT INTO chair_total_distances (chair_id, total_distance, total_distance_updated_at, last_latitude, last_longitude)
SELECT distance_table.chair_id,
       distance_table.total_distance,
       distance_table.total_distance_updated_at,
       chair_locations.latitude,
       chair_locations.longitude
FROM (SELECT chair_id,
             SUM(IFNULL(distance, 0)) AS total_distance,
             MAX(created_at)          AS total_distance_updated_at
      FROM (SELECT chair_id,
                   created_at,
                   ABS(latitude - LAG(latitude) OVER (PARTITION BY chair_id ORDER BY created_at)) +
                   ABS(longitude - LAG(longitude) OVER (PARTITION BY chair_id ORDER BY created_at)) AS distance
            FROM chair_locations) tmp
      GROUP BY chair_id) distance_table
       INNER JOIN chair_locations ON chair_locations.chair_id = distance_table.chair_id
                                 AND chair_locations.created_at = distance_table.total_distance_updated_at;
