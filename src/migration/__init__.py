"""GCS/S3 → S3 distributed file transfer tool over an SQS work queue.

Source backend defaults to S3 (also covers GCS via rclone's
``type=s3 + provider=GCS`` endpoint — see docs/superpowers/specs).
"""

__version__ = "0.1.0"
