//! Shared date helpers. Edookit row/header dates are wall-clock with no offset
//! suffix, so they're anchored to the school's timezone before being formatted
//! as RFC3339. Parsing via jiff's checked civil constructors rejects impossible
//! inputs ("32.13.2026", "31.02.2026") rather than normalizing them.

use jiff::tz::TimeZone;

/// RFC3339 with a colon-separated offset, e.g. `2026-05-21T12:31:00+02:00`.
const RFC3339: &str = "%Y-%m-%dT%H:%M:%S%:z";

/// Assembles an RFC3339 timestamp from civil parts anchored to `tz`. Returns
/// `None` for an impossible date/time.
pub fn civil_to_rfc3339(y: i16, mo: i8, d: i8, h: i8, mi: i8, tz: &TimeZone) -> Option<String> {
    let date = jiff::civil::Date::new(y, mo, d).ok()?;
    let time = jiff::civil::Time::new(h, mi, 0, 0).ok()?;
    let zoned = jiff::civil::DateTime::from_parts(date, time)
        .to_zoned(tz.clone())
        .ok()?;
    Some(zoned.strftime(RFC3339).to_string())
}

/// Converts a unix timestamp (seconds) to RFC3339 in `tz`. "" when ts <= 0
/// (how Edookit represents "no date").
pub fn unix_to_rfc3339(ts: i64, tz: &TimeZone) -> String {
    if ts <= 0 {
        return String::new();
    }
    match jiff::Timestamp::from_second(ts) {
        Ok(t) => t.to_zoned(tz.clone()).strftime(RFC3339).to_string(),
        Err(_) => String::new(),
    }
}
