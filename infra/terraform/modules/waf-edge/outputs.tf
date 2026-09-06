output "alb_arn" {
  value = aws_lb.this.arn
}

output "alb_arn_suffix" {
  value = aws_lb.this.arn_suffix
}

output "alb_dns_name" {
  value = aws_lb.this.dns_name
}

output "alb_zone_id" {
  value = aws_lb.this.zone_id
}

output "target_group_arn" {
  value = aws_lb_target_group.api.arn
}

output "target_group_arn_suffix" {
  value = aws_lb_target_group.api.arn_suffix
}

output "liveness_target_group_arn" {
  value = aws_lb_target_group.api_liveness.arn
}

output "liveness_target_group_arn_suffix" {
  value = aws_lb_target_group.api_liveness.arn_suffix
}

# ALBRequestCountPerTarget needs the load balancer and target group ids in
# "app/<name>/<id>/targetgroup/<name>/<id>" form; both arn_suffixes already
# have exactly that shape.
output "autoscaling_resource_label" {
  value = "${aws_lb.this.arn_suffix}/${aws_lb_target_group.api.arn_suffix}"
}

output "https_listener_arn" {
  value = aws_lb_listener.https.arn
}

output "web_acl_arn" {
  value = aws_wafv2_web_acl.regional.arn
}

output "cloudfront_domain_name" {
  value = one(aws_cloudfront_distribution.this[*].domain_name)
}

output "alb_logs_bucket" {
  value = aws_s3_bucket.alb_logs.bucket
}
