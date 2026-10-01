# Обновления форка

Основная ветка — `main`, upstream — `MetaCubeX/mihomo:Alpha`.
Изменения OLCRTC, Tailscale, редактора провайдеров и adaptive health сохранены.
Связанные задачи: MIHOMO-3, ZASH-1, XKEEN-1.

После push в main workflow `Fork release` проверяет код и сценарий normal → whitelist → normal с восстановлением истории после перезапуска, затем публикует стабильный GitHub Release в `x-happy-x/mihomo`.
Версия `v1.19.32-fork.N` совпадает в бинарнике, имени архива, теге и `version.txt`.
Поддержаны Linux ARM64, ARMv7, amd64-v1, MIPS LE soft/hardfloat, MIPS BE hardfloat и Windows amd64-v1.
Архивы используют стандартные имена встроенного updater и XKeen; к релизу прилагаются SHA-256.
При следующем обновлении upstream нужно также поднять базовую версию в workflow.

Оба канала встроенного обновления (`release` и `alpha`) используют последний стабильный релиз **этого форка**. Отдельного потока upstream Alpha больше нет: upstream попадает сюда только после слияния и проверок.
Размер сжатого пакета ограничен 128 MiB с учётом встроенного Tailscale.

Панель по умолчанию: `https://github.com/x-happy-x/zashboard/releases/latest/download/dist-cdn-fonts.zip`.
Два стандартных rolling URL старого Zashboard (`Zephyruso`, dist.zip и dist-cdn-fonts.zip) автоматически переходят на x-happy-x при загрузке конфигурации. Закреплённые версии и произвольные пользовательские URL сохраняются.

Старый бинарник на устройстве не узнает новые адреса самостоятельно: первое обновление на этот форк нужно выполнить явно. Публикация релиза не изменяет роутер. Adaptive health включается отдельно в YAML, см. [adaptive-health.md](adaptive-health.md).
