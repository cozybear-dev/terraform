resource "fp_thing" "gate" {
  count  = 2
  name   = "gate-${count.index}"
  value  = "gate"
  behave = "sleep:100"
}
