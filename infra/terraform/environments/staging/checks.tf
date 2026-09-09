# ---------------------------------------------------------------------------
# The endpoints a stage must have, given the binaries it runs.
#
# A `validation` block can only see its own variable, and the question here
# spans two: which services are deployed, and which endpoints are configured.
# internal/config/service.go is the same table on the Go side, and a binary that
# reaches AWS without its dependency refuses to start -- so this turns a task
# that crash-loops in production into a plan that fails on a laptop.
# ---------------------------------------------------------------------------

check "workers_have_the_endpoints_they_dial" {
  assert {
    condition     = !contains(keys(var.service_sizing), "relay-worker") || var.redpanda.brokers != ""
    error_message = "relay-worker publishes to the event bus; set redpanda.brokers or do not deploy it."
  }
  assert {
    condition     = !contains(keys(var.service_sizing), "market-ingest-worker") || (var.redpanda.brokers != "" && var.clickhouse.addr != "")
    error_message = "market-ingest-worker writes Solana data through Redpanda into ClickHouse; set both or do not deploy it."
  }
  assert {
    condition     = !contains(keys(var.service_sizing), "workflow-worker") || var.temporal.host_port != ""
    error_message = "workflow-worker hosts Temporal workflows; set temporal.host_port or do not deploy it."
  }
}
