terraform {
  required_providers {
    tls = { source = "hashicorp/tls" }
  }
}

resource "tls_private_key" "ca" {
  algorithm = "RSA"
  rsa_bits  = 4096
}
