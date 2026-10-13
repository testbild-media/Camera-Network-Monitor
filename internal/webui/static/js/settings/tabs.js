// Settings tabs (id, label) – shared by the settings shell and the startup
// screen choice, which must not load the settings code itself.
export const SETTINGS_TABS = [
  ['monitors', 'Monitore / Layouts'],
  ['cameras', 'Kameras'],
  ['groups', 'Kamera-Gruppen'],
  ['devices', 'Geräte'],
  ['displays', 'Displays'],
  ['users', 'Benutzer'],
  ['system', 'System'],
];

export function settingsTabLabel(id) {
  const t = SETTINGS_TABS.find((x) => x[0] === id);
  return t ? t[1] : id;
}
