resource "fp_thing" "old" {
  name  = "old"
  value = "from an older schema"
}

resource "fp_thing" "compact" {
  count = 2
  name  = "compact-${count.index}"
  value = "written without indentation"
  tags  = { "<b>" = "&", "é" = "accent" }
}

resource "fp_thing" "keyed" {
  for_each = toset(["café", "plain"])
  name     = "keyed-${each.key}"
  value    = "non-ASCII instance key"
}

resource "fp_thing" "marked" {
  name   = "marked"
  value  = "has sensitive paths"
  secret = "s3cr3t"
  tags   = { hidden = "s3cr3t", shown = "ok" }
}

resource "fp_thing" "tainted" {
  name  = "tainted"
  value = "was tainted"
}

output "kept" {
  value = "an output that was already in the state"
}
