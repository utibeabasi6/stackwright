resource "tls_self_signed_cert" "example" {
  private_key_pem       = tls_private_key.ca.private_key_pem
  validity_period_hours = 720
  allowed_uses          = ["key_encipherment", "digital_signature", "server_auth"]

  subject {
    common_name = "example.com"
  }
}
