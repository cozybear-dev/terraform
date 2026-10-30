resource "fp_thing" "anything" {
  name  = "anything"
  value = "unchanged throughout"
}

# The same number each time it's evaluated, but held with the precision of a
# 64-bit float, where the copy that's read back from the state is held with
# far more. Upstream writes the two differently.
output "power" {
  value = pow(2, 60)
}

output "powers" {
  value = [pow(2, 53) + 2, pow(2, 62), -pow(2, 61), pow(10, 18)]
}
