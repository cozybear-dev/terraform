variable "name" {
  type = string
  validation {
    condition     = length(var.name) > 2
    error_message = "The name must be longer than two characters."
  }
}

variable "limit" {
  type = number
}

resource "fp_thing" "subject" {
  name  = var.name
  value = "limit ${var.limit}"
  num   = var.limit
  lifecycle {
    precondition {
      condition     = var.limit >= 0
      error_message = "The limit can't be negative."
    }
    postcondition {
      condition     = self.derived != ""
      error_message = "The derived value must not be empty."
    }
  }
}

output "id" {
  value = fp_thing.subject.id
}

output "late" {
  value       = fp_thing.subject.late
  description = "Decided when applied."
}

output "limit_text" {
  value = "limit is ${var.limit}"
  precondition {
    condition     = var.limit < 1000
    error_message = "The limit is too large to report."
  }
}

output "nested" {
  value = {
    list = [fp_thing.subject.id, fp_thing.subject.late]
    map  = { name = var.name }
  }
}
