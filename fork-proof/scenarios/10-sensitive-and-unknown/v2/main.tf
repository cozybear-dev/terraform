variable "password" {
  type      = string
  sensitive = true
  default   = "correct horse"
}

variable "public" {
  default = "still visible"
}

resource "fp_thing" "secretive" {
  name   = "secretive"
  value  = var.public
  secret = var.password
  # One sensitive value among ordinary ones.
  tags = {
    plain  = "seen"
    hidden = var.password
  }
  items = ["a", sensitive("b"), "c"]
  obj   = { a = var.password, b = 1 }
}

# Unknown until the first resource has been applied, and sensitive too.
resource "fp_thing" "downstream" {
  name   = "downstream"
  value  = fp_thing.secretive.late
  secret = "${fp_thing.secretive.late}:${var.password}"
  tags   = { from = fp_thing.secretive.secret }
}

resource "fp_thing" "revealed" {
  name  = "revealed"
  value = nonsensitive(fp_thing.secretive.tags["hidden"])
}

output "sensitive_out" {
  value     = fp_thing.downstream.secret
  sensitive = true
}

output "whole_object" {
  value     = fp_thing.secretive
  sensitive = true
}

output "unknown_until_applied" {
  value = fp_thing.downstream.late
}
