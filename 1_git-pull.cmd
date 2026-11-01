for /F "delims== tokens=1,* eol=#" %%i in (gittoken) do set %%i=%%~j

git pull  https://barlone:%GITTOKEN%@github.com/barlone/blindpostman-bot.git

