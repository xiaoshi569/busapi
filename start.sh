#!/bin/sh

# Start a virtual display for "headful" Chrome in headless servers
Xvfb :99 -screen 0 1280x800x24 >/dev/null 2>&1 &
export DISPLAY=:99

# Run the application
exec /app/business2api

