terraform {
  required_providers {
    local = { source = "hashicorp/local" }
  }
}

# The file exists only in the sandbox that applied it, so the next
# sandbox finds it gone: a change made outside Stackwright.
resource "local_file" "marker" {
  filename = "${path.module}/marker.txt"
  content  = "applied"
}
