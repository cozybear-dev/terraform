variable "fail" {
  default = "true"
}

resource "fp_thing" "provisioned" {
  name  = "provisioned"
  value = "has provisioners"

  provisioner "local-exec" {
    command = "true"
  }

  provisioner "local-exec" {
    when       = destroy
    command    = "true"
    on_failure = continue
  }
}

resource "fp_thing" "fails" {
  name  = "fails"
  value = fp_thing.provisioned.late

  provisioner "local-exec" {
    command = var.fail
  }
}

resource "fp_thing" "after" {
  name  = "after"
  value = fp_thing.fails.late
}
