//! spade-metering: converts service_state_interval rows into hourly meter_reading rows.
//! Readings are keyed (service, sku, period_start) so re-running an hour is idempotent.
use anyhow::Result;
use chrono::{DateTime, Duration, DurationRound, Utc};
use tokio_postgres::{Client, NoTls};

/// Billable thousandths of an hour that [start,end) overlaps the hour beginning at `hour`.
pub fn overlap_milli(start: DateTime<Utc>, end: Option<DateTime<Utc>>, hour: DateTime<Utc>, now: DateTime<Utc>) -> i64 {
    let h_end = hour + Duration::hours(1);
    let s = start.max(hour);
    let e = end.unwrap_or(now).min(h_end);
    if e <= s {
        return 0;
    }
    ((e - s).num_milliseconds() * 1000 / 3_600_000).max(0)
}

async fn meter_hour(db: &Client, hour: DateTime<Utc>, now: DateTime<Utc>) -> Result<u64> {
    let rows = db
        .query(
            "SELECT i.service_id::text, i.started_at, i.ended_at, z.vcpu, z.ram_mb, z.disk_gb, s.support_plan
             FROM service_state_interval i JOIN service s ON s.id=i.service_id JOIN size z ON z.id=s.size_id
             WHERE i.started_at < $1 AND (i.ended_at IS NULL OR i.ended_at > $2)",
            &[&(hour + Duration::hours(1)), &hour],
        )
        .await?;
    let mut acc: std::collections::BTreeMap<(String, String), i64> = Default::default();
    for r in rows {
        let svc: String = r.get(0);
        let frac = overlap_milli(r.get(1), r.get(2), hour, now);
        let (vcpu, ram_mb, disk_gb, plan): (i32, i32, i32, String) = (r.get(3), r.get(4), r.get(5), r.get(6));
        let mut add = |sku: String, units_milli: i64| *acc.entry((svc.clone(), sku)).or_default() += units_milli;
        add("mgmt.vcpu.hour".into(), frac * vcpu as i64);
        add("mgmt.ram_gb.hour".into(), frac * ram_mb as i64 / 1024);
        add("mgmt.disk_gb.hour".into(), frac * disk_gb as i64);
        if plan != "L1" {
            add(format!("support.{plan}.hour"), frac);
        }
    }
    let mut n = 0;
    for ((svc, sku), q) in acc {
        if q == 0 {
            continue;
        }
        n += db
            .execute(
                "INSERT INTO meter_reading(service_id, sku, period_start, quantity_milli) VALUES ($1::text::uuid,$2,$3,$4)
                 ON CONFLICT (service_id, sku, period_start) DO NOTHING",
                &[&svc, &sku, &hour, &q],
            )
            .await?;
    }
    Ok(n)
}

#[tokio::main]
async fn main() -> Result<()> {
    let url = std::env::var("SPADE_DATABASE_URL").unwrap_or("postgres://spade:spade-local-dev@localhost:5433/spadeai".into());
    let (db, conn) = tokio_postgres::connect(&url, NoTls).await?;
    tokio::spawn(async move {
        if let Err(e) = conn.await {
            eprintln!("postgres connection error: {e}");
        }
    });
    let once = std::env::args().any(|a| a == "--once");
    loop {
        let now = Utc::now();
        let current = now.duration_trunc(Duration::hours(1))?;
        let n = catch_up(&db, current, now).await?;
        if n > 0 {
            println!("{{\"metered_readings\":{n}}}");
        }
        if once {
            return Ok(());
        }
        tokio::time::sleep(std::time::Duration::from_secs(300)).await;
    }
}

/// Hours to meter: every closed hour after the cursor, up to `MAX_HOURS_PER_PASS` per pass.
/// Without a cursor, start at the earliest billable interval so no history is skipped.
pub fn pending_hours(cursor: Option<DateTime<Utc>>, earliest: Option<DateTime<Utc>>, current: DateTime<Utc>) -> Vec<DateTime<Utc>> {
    let first = match (cursor, earliest) {
        (Some(c), _) => c + Duration::hours(1),
        (None, Some(e)) => e.duration_trunc(Duration::hours(1)).unwrap_or(e),
        (None, None) => return vec![],
    };
    let mut out = vec![];
    let mut h = first;
    while h < current && out.len() < MAX_HOURS_PER_PASS {
        out.push(h);
        h += Duration::hours(1);
    }
    out
}

const MAX_HOURS_PER_PASS: usize = 24 * 31;

async fn catch_up(db: &Client, current: DateTime<Utc>, now: DateTime<Utc>) -> Result<u64> {
    let cursor: Option<DateTime<Utc>> = db.query_opt("SELECT last_hour FROM metering_cursor", &[]).await?.map(|r| r.get(0));
    let earliest: Option<DateTime<Utc>> = db.query_one("SELECT min(started_at) FROM service_state_interval", &[]).await?.get(0);
    let mut n = 0;
    for hour in pending_hours(cursor, earliest, current) {
        n += meter_hour(db, hour, now).await?;
        db.execute(
            "INSERT INTO metering_cursor(id,last_hour) VALUES (true,$1) ON CONFLICT (id) DO UPDATE SET last_hour=EXCLUDED.last_hour, updated_at=now()",
            &[&hour],
        )
        .await?;
    }
    Ok(n)
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;
    #[test]
    fn overlap_is_clamped_to_hour() {
        let h = Utc.with_ymd_and_hms(2026, 1, 1, 10, 0, 0).unwrap();
        let now = h + Duration::hours(5);
        assert_eq!(overlap_milli(h - Duration::hours(2), None, h, now), 1000);
        assert_eq!(overlap_milli(h + Duration::minutes(30), None, h, now), 500);
        assert_eq!(overlap_milli(h + Duration::minutes(15), Some(h + Duration::minutes(45)), h, now), 500);
        assert_eq!(overlap_milli(h + Duration::hours(2), None, h, now), 0);
    }
    #[test]
    fn pending_hours_backfills_from_cursor_without_gap() {
        let cur = Utc.with_ymd_and_hms(2026, 1, 2, 0, 0, 0).unwrap();
        let hrs = pending_hours(Some(cur - Duration::hours(10)), None, cur);
        assert_eq!(hrs.len(), 9);
        assert_eq!(hrs[0], cur - Duration::hours(9));
        assert_eq!(*hrs.last().unwrap(), cur - Duration::hours(1));
        let first = pending_hours(None, Some(cur - Duration::minutes(150)), cur);
        assert_eq!(first[0], cur - Duration::hours(3));
        assert!(pending_hours(None, None, cur).is_empty());
        assert!(pending_hours(Some(cur - Duration::hours(1)), None, cur).is_empty());
    }
}
