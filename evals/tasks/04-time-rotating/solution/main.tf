terraform {
  required_providers {
    time = { source = "hashicorp/time" }
  }
}

resource "time_rotating" "key_rotation" {
  rotation_days = 90
}
