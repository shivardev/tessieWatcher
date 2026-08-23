# Bug log

Every correctness bug found in teslalog during its first days of real
use, with what caused it. Kept because the *causes* cluster into a
small number of repeatable mistakes, and naming those is more useful
than the individual fixes.

---

## The patterns

Four root causes account for nearly every bug below.

**P1 — Reconstructing TeslaMate's behavior instead of reading its
source.** TeslaMate is open source. Where its behavior was inferred
from documentation, dashboards, or reasoning about what it "probably"
does, the inference was frequently wrong in a way that looked
plausible. Every value checked directly against
`lib/teslamate/vehicles/vehicle.ex` or `lib/teslamate/log.ex` was
right; several that weren't, weren't.

**P2 — Zero as a stand-in for unknown.** Go's zero values are `0`,
`false`, `""`. Wherever a value that could genuinely be *unknown* was
stored in a non-pointer type, "we didn't observe this" became a
confident, wrong measurement.

**P3 — A rule that only fires in a state the system rarely reaches.**
Logic gated on a condition that seemed like the natural moment for it,
but which real-world state transitions almost never satisfy.

**P4 — Plausible substitution.** Using a field, source, or method that
reads as equivalent to the correct one and is not.

---

## Polling and state machine

**Idle timeout was 3 minutes; TeslaMate's is 15.** (P1) Gave up active
polling five times sooner than TeslaMate, missing activity during any
errand stop longer than a few minutes. The value was reasoned about
rather than read; TeslaMate's lives in
`car_settings.suspend_after_idle_min`.

**Suspended check interval was 15 minutes; TeslaMate's is 21.** (P1)
Same cause, same file (`suspend_min`).

**OFFLINE was polled on the ASLEEP schedule.** (P1, P3) Both were
bucketed as "asleep-like" and checked every 15 minutes, reasoning that
neither needs attention. But ASLEEP means *leave the car alone*;
OFFLINE just means the last check couldn't reach it — including in the
seconds before a drive begins. A real drive lost its first ~10 minutes
and ~11 km. TeslaMate uses a flat 30 s `@asleep_interval` for both.

**Neutral (N) wasn't treated as driving.** (P1) TeslaMate's guard is
`shift_state in ~w(D N R)`. Coasting in neutral ended the drive and
started a new one on the far side.

**No Updating state.** (P1) A software install outlasts the idle
timeout, so the daemon suspended mid-install — and the cheap check
can't observe an install finishing, so updates could go permanently
unrecorded.

**Drive/charge abandonment fired instantly.** (P1) TeslaMate waits
`@drive_timeout_min` (15 min) measured from the last real observation,
and never auto-closes a charge that merely went offline. Firing
instantly split single drives in two across ordinary tunnel or garage
signal loss.

**A drive could be silently merged into a later one.** (P3) Worse than
a row staying open: the internal "mid-drive" flag was never reset when
the vehicle vanished, so the next drive — hours later — was recorded as
a continuation of the abandoned one.

---

## Data capture

**Every drive and charge lost its first and last sample.** (P1) Only
the "point" events wrote telemetry; start and end events just opened
and closed the row. TeslaMate calls `insert_position` / `insert_charge`
on the first and last observation too. A short two-poll drive recorded
zero positions.

**~90% of samples recorded fake zeros.** (P2) Fields the streaming
protocol doesn't carry — fan status, climate, TPMS, usable battery %,
ideal/estimated range, sentry/valet — were non-pointer types, so every
streaming-derived sample (the large majority) wrote `0`/`false` rather
than NULL. Indistinguishable from a real reading. Fixed at the type
level so it can't silently regress.

**`battery_heater` was never captured.** (P4) A code comment asserted
Tesla had no such field separate from `battery_heater_on`. It does — in
`climate_state`, not `charge_state` — and the two genuinely disagree:
245 position samples and 218 charge samples in real recorded data have
`battery_heater` true while `battery_heater_on` is false. Found by
diffing a full TeslaMate `pg_dump`'s schema, then checking whether the
missing columns actually carried distinct data.

**No minimum-drive filter.** (P1) TeslaMate discards a drive with
fewer than 2 positions or under 10 m. Without it, every bumped shifter
and GPS jitter became a permanent row.

---

## Charging and cost

**Free supercharging keyed on `fast_charger_brand`.** (P4) Brand reads
"Tesla" for a home Tesla wall connector — ~98,000 of ~104,000 real
samples. `fast_charger_type` reads "Tesla" only at an actual
Supercharger (~100). With free supercharging enabled this would have
marked every home charge free and zeroed the entire electricity bill.
The brand field reads more naturally for "was this a Tesla charger",
which is exactly why it was wrong.

**Charger type came from the last sample.** (P4) TeslaMate takes the
statistical mode across samples where power was flowing. `<invalid>`
appears in 5,103 real samples, so any session ending on one was
misclassified.

**The end-of-charge zero reading was trusted.** (P1) Tesla sometimes
reports `charge_energy_added` as exactly 0 on the final sample.
TeslaMate guards with `coalesce(nullif(last_value, 0), max(...))`.

**Cost was a single global rate.** (P1) Real rates differ per
location. Three configured locations here span $0.125/kWh, $0.30/kWh
plus a $0.45 session fee, and free — none of which one number can
express. Per-kWh billing must also charge the *greater* of energy added
and energy drawn from the wall; billing energy added alone understates
every charge.

**The geofence was lost on restart mid-charge.** (P2) Held only in
memory, so a restart during a charge silently fell back to the global
rate. TeslaMate persists `geofence_id` on the row.

---

## Efficiency

**The config seed clobbered the derived figure on every restart.**
`[vehicle].efficiency_wh_km` is documented as a starting point that
teslalog replaces once it derives the real value from charging history
— but it was reapplied unconditionally at startup, overwriting the
derived value with the guess forever. Latent only because the value
was 0.

**The offline-charge estimate read the config value directly,** using
the guess even when a derived figure existed.

---

## Location resolution

**The backfill sweep was structurally starved.** (P3) It ran only
while the vehicle was asleep/offline, reasoning that nothing else
competes for the geocoder's rate limit then. But a parked car keeps
reporting *online* for hours, so the state machine cycles
`idle → suspended → online → idle`, and the check that decides whether
to sweep always lands immediately after a summary check has flipped the
state back out of asleep-like. The sweep essentially never fired, and
blank from/to columns stayed blank indefinitely — during exactly the
long idle stretches it was designed for. Only visible with shell access
to watch it not happen.

**Overlapping geofences resolved by config order,** not by distance, so
a small zone nested inside a larger one depended on file ordering.

---

## Not bugs (investigated and cleared)

**Efficiency reading 0.0 Wh/km.** Correct: TeslaMate derives 131.4
Wh/km from 98 qualifying charges over months; teslalog had 2, which
disagree (0.15 vs 0.13 kWh/km), so no precision threshold qualifies.
Same algorithm, insufficient data — teslalog's second charge computes
0.1315 against TeslaMate's 0.1314.

**Streaming disconnects every ~5 minutes.** Tesla's server-side
timeout. Reconnects take 3–7 s and REST polling covers the gap:
measured max position gap during a drive is 5.8 s against a 0.29 s
average.

**Distances 0–80 m higher than TeslaMate's.** teslalog's start odometer
is the last known value from while the car was parked; TeslaMate's is
its first *recorded position* of the drive, already 42–80 m in.
teslalog captures slightly more of each drive, and consistently records
more positions.

---

## Known bad historical data

The drive at **2026-08-22 01:55** is recorded as 3.64 km. The real
drive was 14.82 km — teslalog caught only the last quarter, due to the
OFFLINE polling bug above. The missing telemetry was never captured and
cannot be reconstructed. `teslamate-import` can restore that period
from a TeslaMate instance if a clean history matters more than keeping
teslalog's own record.

---

## Method notes

Two false alarms came from throwaway diagnostic scripts that discarded
errors: a SQL query with an ambiguous column name, and a Go scan of a
NULL into a non-nullable `float64` that left Go zero values behind,
which read as "empty data". Both briefly looked like real product bugs.
Diagnostic code that decides whether something is broken deserves the
same error checking as the product.
