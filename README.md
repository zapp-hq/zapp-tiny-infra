# zapp-tiny-infra

A. Go Application Hosting (PaaS):

Render.com:

Free Tier: Offers a generous free tier for web services (750 instance hours/month, 100GB bandwidth, 500 build minutes).
Pros: Very easy to deploy from Git, supports Go directly, managed environment.
Cons: Free web services spin down after inactivity (can cause cold starts), no persistent disk on free tier (not an issue for your stateless Go server).
How to: Connect your GitHub repo, select Go, configure build command (go build -o server .) and start command (./server). Set environment variables for REDIS_ADDR.

B. Redis Database Hosting (Managed Free Tier):

Upstash (Recommended for Serverless/Ephemeral):
Free Tier: Offers a serverless Redis free tier with 10,000 commands/day, 256MB max dataset size, and 50MB data transfer/day. Perfect for your ephemeral Zapp! data.
Pros: Designed for serverless/edge functions, very low latency, easy to set up.
Cons: Limits on commands/data transfer, not suitable for very high-volume or large datasets.
How to: Sign up, create a new Redis database. You'll get a URL and password to configure in your Go app's REDIS_ADDR environment variable.
