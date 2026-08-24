import initSqlJs from 'sql.js'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const frontendDirectory = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const source = await readFile(
  resolve(frontendDirectory, '..', 'internal/storage/schema.go'),
  'utf8',
)
const schema = source.match(/const schema = `([\s\S]*?)`/)?.[1]
if (!schema) throw new Error('Canonical schema not found')
const SQL = await initSqlJs()
const database = new SQL.Database()
database.exec(schema)
database.run(
  "INSERT INTO vehicles (vin,display_name,model,marketing_name,firmware_version,efficiency_wh_km) VALUES ('TESTVIN','Roadrunner','Y','Model Y Long Range','2026.20.6',155)",
)
database.run(
  "INSERT INTO states (vehicle_id,state,started_at) VALUES (1,'asleep','2026-08-21T20:00:00.000Z')",
)
database.run(
  "INSERT INTO battery_samples (vehicle_id,timestamp,battery_level,battery_range_km,ideal_battery_range_km,source) VALUES (1,'2026-08-21T20:00:00.000Z',72,355,370,'fixture'),(1,'2026-08-21T21:00:00.000Z',71,350,365,'fixture')",
)
database.run(
  "INSERT INTO drives (vehicle_id,start_time,end_time,start_odometer_km,end_odometer_km,distance_km,duration_min,start_battery_level,end_battery_level,start_range_km,end_range_km,start_lat,start_lng,end_lat,end_lng,start_location,end_location,max_speed_kmh,ascent_m,descent_m,status) VALUES (1,'2026-08-21T18:00:00.000Z','2026-08-21T18:25:00.000Z',7400,7418,18,25,78,72,390,355,35.04,-85.15,35.10,-85.06,'Home','Park',96,120,80,'closed')",
)
database.run(
  "INSERT INTO positions (drive_id,vehicle_id,timestamp,latitude,longitude,speed_kmh,power_kw,elevation_m,battery_level,outside_temp_c,inside_temp_c,tpms_pressure_fl,tpms_pressure_fr,tpms_pressure_rl,tpms_pressure_rr) VALUES (1,1,'2026-08-21T18:00:00.000Z',35.04,-85.15,0,2,210,78,29,24,2.8,2.8,2.9,2.9),(1,1,'2026-08-21T18:12:00.000Z',35.07,-85.10,72,18,245,75,30,24,2.8,2.8,2.9,2.9),(1,1,'2026-08-21T18:25:00.000Z',35.10,-85.06,0,-2,225,72,30,24,2.8,2.8,2.9,2.9)",
)
database.run(
  "INSERT INTO charging_sessions (vehicle_id,start_time,end_time,start_battery_level,end_battery_level,charge_energy_added_kwh,charge_energy_used_kwh,max_charger_power_kw,cost,latitude,longitude,location,is_dc_fast_charge,status) VALUES (1,'2026-08-20T02:00:00.000Z','2026-08-20T05:00:00.000Z',40,80,28,31,11,3.92,35.04,-85.15,'Home',0,'closed')",
)
database.run(
  "INSERT INTO software_updates (vehicle_id,version,status,start_time,end_time) VALUES (1,'2026.20.6','installed','2026-08-18T01:00:00.000Z','2026-08-18T01:30:00.000Z')",
)
const outputDirectory = resolve(frontendDirectory, 'test-data')
await mkdir(outputDirectory, { recursive: true })
await writeFile(resolve(outputDirectory, 'sample.db'), database.export())
database.close()
console.log('Created test-data/sample.db')
