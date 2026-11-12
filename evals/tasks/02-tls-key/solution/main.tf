terraform {
  required_providers {
    tls = { source = "hashicorp/tls" }
  }
}

resource "tls_private_key" "ca" {
  algorithm   = "ECDSA"
  ecdsa_curve = "P384"
}
