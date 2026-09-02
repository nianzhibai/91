/**
 * 详情页播放器的倍速档位与文案。
 *
 * ArtPlayer 自带的倍速入口只有两个：桌面端右键菜单，以及设置面板里的
 * “播放速度”二级菜单；右键菜单在移动端根本不会初始化。控制栏上新增的倍速
 * 按钮必须和这两处共用同一份档位，否则同一个播放器会出现两套倍速。
 */

/** 与 Artplayer.PLAYBACK_RATE 一致的倍速档位。 */
export const PLAYBACK_RATE_OPTIONS = [0.5, 0.75, 1, 1.25, 1.5, 2];

/** 正常倍速。 */
export const NORMAL_PLAYBACK_RATE = 1;

/**
 * 读回来的 playbackRate 可能是 0 / NaN / 负数（媒体元素重载、长按倍速被
 * 打断等），一律回落到正常倍速再参与比较和展示。
 */
export function normalizePlaybackRate(rate: unknown): number {
  const value = Number(rate);
  if (!Number.isFinite(value) || value <= 0) return NORMAL_PLAYBACK_RATE;
  return value;
}

export function isNormalPlaybackRate(rate: unknown) {
  return normalizePlaybackRate(rate) === NORMAL_PLAYBACK_RATE;
}

/** 控制栏按钮上的文案：正常倍速显示“倍速”，其它显示实际倍数。 */
export function formatPlaybackRateLabel(rate: unknown) {
  return isNormalPlaybackRate(rate) ? "倍速" : `${formatRateNumber(rate)}x`;
}

/** 下拉列表里的文案：与设置面板一致，正常倍速显示“正常”。 */
export function formatPlaybackRateOptionLabel(rate: unknown) {
  return isNormalPlaybackRate(rate) ? "正常" : `${formatRateNumber(rate)}x`;
}

/**
 * 把当前倍速对齐到列表里的档位；长按倍速等临时值不在档位里时返回 null，
 * 让调用方把所有选项的选中态清掉，而不是硬标一个不对的档位。
 */
export function matchPlaybackRateOption(rate: unknown): number | null {
  const value = normalizePlaybackRate(rate);
  return PLAYBACK_RATE_OPTIONS.includes(value) ? value : null;
}

function formatRateNumber(rate: unknown) {
  // 0.75 不能像 ArtPlayer 右键菜单那样按 toFixed(1) 显示成 0.8，
  // 那个数字和真实倍速对不上。
  return Number(normalizePlaybackRate(rate).toFixed(2)).toString();
}
