variable "generation" {
  default = 1
}

# Not declared as created before it's destroyed, but something that is
# depends on it, which makes it so.
resource "fp_thing" "needed" {
  name  = "needed-${var.generation}"
  value = "replaced by creating first, because of what depends on it"
}

resource "fp_thing" "cbd" {
  name  = "cbd-${var.generation}"
  value = fp_thing.needed.late
  lifecycle {
    create_before_destroy = true
  }
}

resource "fp_thing" "plain" {
  name  = "plain-${var.generation}"
  value = "replaced by destroying first"
}

# Depends on the one that is created first, and is itself replaced in the
# ordinary order.
resource "fp_thing" "follows" {
  name  = "follows-${fp_thing.cbd.id}"
  value = fp_thing.cbd.late
}

resource "fp_thing" "user" {
  count = 2
  name  = "user-${count.index}"
  value = "${fp_thing.cbd.late} ${fp_thing.plain.late}"
}
