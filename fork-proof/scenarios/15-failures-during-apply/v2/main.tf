variable "broken" {
  default = "fail_plan"
}

resource "fp_thing" "before" {
  name  = "before"
  value = "created before the failure"
}

resource "fp_thing" "breaks" {
  name   = "breaks"
  value  = fp_thing.before.late
  behave = var.broken
}

# These depend on the one that fails, directly and indirectly, so they must
# not be attempted.
resource "fp_thing" "after" {
  count = 3
  name  = "after-${count.index}"
  value = fp_thing.breaks.late
}

resource "fp_thing" "much_later" {
  name  = "much-later"
  value = fp_thing.after[2].late
}

# These don't, so they must be created regardless.
resource "fp_thing" "unrelated" {
  count = 5
  name  = "unrelated-${count.index}"
  value = "independent"
}

output "reached" {
  value = fp_thing.much_later.id
}
