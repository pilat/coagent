package sessionstore

// SleepScheduleSQL identifies exact-call sleeps over unqualified schedules columns.
const SleepScheduleSQL = `one_shot_at IS NOT NULL AND COALESCE(json_extract(metadata, '$.tool_call_id'), '') <> ''`
