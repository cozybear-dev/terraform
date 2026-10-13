variable "input" {
  default = "second"
}

# Read during planning, because nothing about it is unknown.
data "fp_data" "static" {
  input = var.input
}

resource "fp_thing" "source" {
  name  = "source"
  value = data.fp_data.static.result
}

# Can't be read until the resource it refers to has been applied.
data "fp_data" "derived" {
  input = fp_thing.source.late
}

# Reads nothing unknown, but must wait for the resource regardless.
data "fp_data" "ordered" {
  input      = "ordered"
  depends_on = [fp_thing.source]
}

resource "fp_thing" "consumer" {
  count = 3
  name  = "consumer-${count.index}"
  value = "${data.fp_data.derived.result}/${data.fp_data.ordered.length}"
}

resource "fp_thing" "waits" {
  name       = "waits"
  value      = "no references"
  depends_on = [fp_thing.consumer, data.fp_data.ordered]
}

output "derived" {
  value = data.fp_data.derived.result
}
