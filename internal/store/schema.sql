-- skycam's database. CREATE IF NOT EXISTS throughout: Open applies this on
-- every start. Times are unix milliseconds, UTC.

-- One row per still, saved or not. Nullable REALs are NULL when the value
-- is unknown (no sensor reading, no clear-sky baseline, roof shut).
CREATE TABLE IF NOT EXISTS frames (
    id          INTEGER PRIMARY KEY,
    taken_at    INTEGER NOT NULL,
    night       TEXT    NOT NULL,          -- YYYYMMDD, allsky's night date
    mode        TEXT    NOT NULL,          -- day | night
    file        TEXT    NOT NULL DEFAULT '', -- name under images/<night>/, '' if not kept
    exposure_us INTEGER NOT NULL,
    gain        INTEGER NOT NULL,
    temp_c      REAL,
    sun_alt     REAL    NOT NULL,
    moon_alt    REAL    NOT NULL,
    moon_illum  REAL    NOT NULL,
    mean        REAL    NOT NULL,          -- metered brightness, 0-255
    band_lum    REAL    NOT NULL,          -- skyq's luminance band
    sky_level   REAL    NOT NULL,          -- median background, ADU
    sky_mag     REAL,                      -- relative, not SQM
    stars       INTEGER NOT NULL,
    cloud       REAL,                      -- 0-1
    roof        TEXT    NOT NULL,
    rain        TEXT    NOT NULL,
    nina_mount  TEXT    NOT NULL DEFAULT '',
    nina_filter TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS frames_night ON frames (night, taken_at);
CREATE INDEX IF NOT EXISTS frames_taken ON frames (taken_at);

-- One row per night once its dawn products exist. File columns are names
-- under products/<night>/; '' means not made.
CREATE TABLE IF NOT EXISTS nights (
    night        TEXT    PRIMARY KEY,
    processed_at INTEGER NOT NULL,
    frames       INTEGER NOT NULL DEFAULT 0,
    timelapse    TEXT    NOT NULL DEFAULT '',
    keogram      TEXT    NOT NULL DEFAULT '',
    startrails   TEXT    NOT NULL DEFAULT '',
    report       TEXT    NOT NULL DEFAULT '',
    uploaded_at  INTEGER,                  -- S3
    published_at INTEGER                   -- report to the relay
);

-- Things the report should mention: live sessions (a gap in the stills),
-- camera errors.
CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY,
    night      TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    note       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_night ON events (night);
