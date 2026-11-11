terraform {
  backend "http" {}
  required_providers {
    time = {
      source  = "hashicorp/time"
      version = "0.13.1"
    }
  }
}

resource "time_sleep" "wait" {
  create_duration = "300s"
}
