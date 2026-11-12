resource "aws_route53_zone" "example" {
  name = "example.org"
}

resource "aws_route53_record" "www" {
  zone_id = aws_route53_zone.example.zone_id
  name    = "www.example.org"
  type    = "A"
  ttl     = 300
  records = ["203.0.113.10"]
}
