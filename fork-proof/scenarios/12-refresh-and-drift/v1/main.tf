resource "fp_thing" "drifts" {
  name   = "drifts"
  value  = "configured"
  behave = "drift"
}

resource "fp_thing" "steady" {
  count = 4
  name  = "steady-${count.index}"
  value = "configured"
}

resource "fp_thing" "reader" {
  name  = "reader"
  value = fp_thing.drifts.derived
}

output "seen" {
  value = fp_thing.drifts.value
}
