-- name: InsertFrame :exec
INSERT INTO frames (
    taken_at, night, mode, file, exposure_us, gain, temp_c, sun_alt, moon_alt,
    moon_illum, mean, band_lum, sky_level, sky_mag, stars, cloud, roof, rain,
    nina_mount, nina_filter
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?, ?, ?, ?, ?,
    ?, ?
);

-- name: FramesForNight :many
SELECT * FROM frames WHERE night = ? AND mode = 'night' ORDER BY taken_at;

-- name: ClearSkyStars :many
-- Star counts from dark, roof-open frames, newest first: the pool the
-- clear-sky baseline is drawn from.
SELECT stars FROM frames
WHERE mode = 'night' AND roof = 'OPEN' AND sun_alt < -12 AND taken_at >= ?
ORDER BY taken_at DESC
LIMIT ?;

-- name: LastNightFrame :one
SELECT night, taken_at FROM frames WHERE mode = 'night' ORDER BY taken_at DESC LIMIT 1;

-- name: DeleteFramesBefore :exec
DELETE FROM frames WHERE taken_at < ?;

-- name: UpsertNight :exec
INSERT INTO nights (night, processed_at, frames, timelapse, keogram, startrails, report)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (night) DO UPDATE SET
    processed_at = excluded.processed_at,
    frames       = excluded.frames,
    timelapse    = excluded.timelapse,
    keogram      = excluded.keogram,
    startrails   = excluded.startrails,
    report       = excluded.report;

-- name: GetNight :one
SELECT * FROM nights WHERE night = ?;

-- name: ListNights :many
SELECT * FROM nights ORDER BY night DESC LIMIT ?;

-- name: MarkNightUploaded :exec
UPDATE nights SET uploaded_at = ? WHERE night = ?;

-- name: MarkNightPublished :exec
UPDATE nights SET published_at = ? WHERE night = ?;

-- name: NightsNotPublished :many
SELECT * FROM nights WHERE published_at IS NULL OR uploaded_at IS NULL ORDER BY night;

-- name: DeleteNight :exec
DELETE FROM nights WHERE night = ?;

-- name: StartEvent :one
INSERT INTO events (night, kind, started_at, note) VALUES (?, ?, ?, ?) RETURNING id;

-- name: EndEvent :exec
UPDATE events SET ended_at = ? WHERE id = ?;

-- name: EventsForNight :many
SELECT * FROM events WHERE night = ? ORDER BY started_at;
