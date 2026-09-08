-- +goose Up

DELETE FROM session_outbox
WHERE state <> 'delivered'
  AND source_key GLOB 'schedule:bgp_*:announcement'
  AND json_extract(attributes, '$.source') = 'scheduler';
