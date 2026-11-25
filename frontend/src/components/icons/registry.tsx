// The application imports semantic roles here. All glyph geometry comes from the pinned OpenBridge package.
import { forwardRef, type SVGProps } from "react";
import {
  alertOffGoogleIcon,
  notificationIcon,
  warningGoogleIcon,
} from "@openships/openbridge-icons/alert";
import {
  conningIecIcon,
  ecdisIecIcon,
  homeIcon,
  lightsIcon,
} from "@openships/openbridge-icons/application";
import {
  fanStaticIcon,
  fireIcon,
  tempColdIcon,
  tempHotcoldIcon,
} from "@openships/openbridge-icons/automation";
import {
  cursorRemovePointIconIcon,
} from "@openships/openbridge-icons/cursors";
import {
  batteryHorizontal100Icon,
  boltGoogleIcon,
  cameraOffIcon,
  cellFullIcon,
  comCallGoogleIcon,
  comMessageGoogleIcon,
  comPaIcon,
  comRadioIcon,
  connection4BarsIcon,
  connectionOffIcon,
  sensorGpsFullIcon,
  sensorWaterDropGoogleIcon,
  soundIcon,
  soundMutedIcon,
  wifiOffGoogleIcon,
  wifiStrength4GoogleIcon,
} from "@openships/openbridge-icons/devices-and-sensors";
import {
  chartLayersIcon,
  commandLockedIcon,
  measureGoogleIcon,
  signExitIcon,
  tankIcon,
  userGoogleIcon,
  userProfilesIcon,
} from "@openships/openbridge-icons/maritime";
import {
  applicationOpenGoogleIcon,
  applicationsIcon,
  arrowBidirectionalHorizontalIcon,
  arrowBidirectionalVerticalIcon,
  arrowDownGoogleIcon,
  arrowLeftGoogleIcon,
  arrowRightGoogleIcon,
  arrowUpGoogleIcon,
  attachmentIcon,
  calendarDateGoogleIcon,
  calendarGoogleIcon,
  cameraIcon,
  checkGoogleIcon,
  checkboxCheckedIcon,
  checkboxUncheckGoogleIcon,
  chevronDoubleLeftGoogleIcon,
  chevronDoubleRightGoogleIcon,
  chevronDownGoogleIcon,
  chevronLeftGoogleIcon,
  chevronRightGoogleIcon,
  chevronUpGoogleIcon,
  clearIcon,
  clipboardIcon,
  closeGoogleIcon,
  codeGoogleIcon,
  compareGoogleIcon,
  completedIcon,
  computerPcIcon,
  computerServerIcon,
  configureIcon,
  contentCollapseGoogleIcon,
  contentCopyGoogleIcon,
  contentCutGoogleIcon,
  contentExpandGoogleIcon,
  databaseIcon,
  deleteIcon,
  diagnosticGoogleIcon,
  dragVerticalIcon,
  editGoogleIcon,
  equalIcon,
  errorIcon,
  ethernetGoogleIcon,
  fileDownloadGoogleIcon,
  fileUploadGoogleIcon,
  filterIcon,
  foodGoogleIcon,
  historyGoogleIcon,
  imageIcon,
  inProgress1Icon,
  infoIcon,
  inputKeyboardGoogleIcon,
  inputRightIcon,
  licenseGoogleIcon,
  limitsInsideIcon,
  linkIcon,
  linkRemoveIcon,
  listAltCheckGoogleIcon,
  logEditGoogleIcon,
  logOpenGoogleIcon,
  mediaLibraryIcon,
  mediaLoopIcon,
  mediaPauseIcon,
  mediaPlayIcon,
  mediaRecordIcon,
  mediaSkipNextIcon,
  mediaSkipPreviousIcon,
  mediaStopIcon,
  menuIcon,
  monitoringIcon,
  moreGoogleIcon,
  moreVerticalGoogleIcon,
  moveItemInIcon,
  notAllowedIcon,
  notStartedIcon,
  offIecIcon,
  onIecIcon,
  onOffIecIcon,
  paletteColorCalibratedIecIcon,
  paletteDayIcon,
  paletteNightIcon,
  panelRightCloseIcon,
  panelRightOpenIcon,
  passedFillIcon,
  passedIcon,
  pendingIcon,
  pinGoogleIcon,
  printIcon,
  redoIecIcon,
  refreshGoogleIcon,
  reorderHorizontalGoogleIcon,
  saveProposalIcon,
  screenFullExitGoogleIcon,
  screenFullGoogleIcon,
  screenPadIcon,
  screenQuadIcon,
  screenRecordingIcon,
  screenSharingIcon,
  screenShotIcon,
  screenSplitLeftIcon,
  searchIcon,
  sendIcon,
  settingsDefaultIecIcon,
  settingsIecIcon,
  simulationIcon,
  sortGoogleIcon,
  speedIcon,
  speedLowIcon,
  starIcon,
  starSelectedIcon,
  tableIcon,
  textIconIcon,
  timeIcon,
  timerGoogleIcon,
  touchIcon,
  transformMoveIcon,
  trendDownIcon,
  trendIcon,
  trendUpIcon,
  trophyIcon,
  undoIecIcon,
  usbIcon,
  visibilityOffGoogleIcon,
  visibilityOnGoogleIcon,
  widgetAddGoogleIcon,
  widgetsIcon,
  wrenchIcon,
} from "@openships/openbridge-icons/ui";
import {
  windIcon,
} from "@openships/openbridge-icons/weather-and-environment";
type IconProps = SVGProps<SVGSVGElement> & { size?: string | number };
export type InterfaceIcon = ReturnType<typeof makeIcon>;

function makeIcon(role: string, artwork: string) {
  // Only static, trusted package exports are passed to this factory.
  const viewBox = artwork.match(/viewBox="([^"]+)"/)?.[1] ?? '0 0 24 24';
  const content = { __html: artwork.slice(artwork.indexOf('>') + 1, artwork.lastIndexOf('</svg>')) };
  const Component = forwardRef<SVGSVGElement, IconProps>(function InterfaceSymbol(
    { size = 24, className = '', color = 'currentColor', ...props }, ref,
  ) {
    return <svg ref={ref} aria-hidden={props['aria-label'] || props['aria-labelledby'] ? undefined : true}
      width={size} height={size} color={color} fill="none" viewBox={viewBox}
      className={`ob-icon ob-icon-${role} ${className}`} {...props} dangerouslySetInnerHTML={content} />;
  });
  Component.displayName = role;
  return Component;
}

export const Activity = /* @__PURE__ */ makeIcon('activity', monitoringIcon);
export const AirVent = /* @__PURE__ */ makeIcon('air-vent', fanStaticIcon);
export const AlertCircle = /* @__PURE__ */ makeIcon('alert-circle', errorIcon);
export const AlertTriangle = /* @__PURE__ */ makeIcon('alert-triangle', warningGoogleIcon);
export const AlignCenter = /* @__PURE__ */ makeIcon('align-center', textIconIcon);
export const AlignLeft = /* @__PURE__ */ makeIcon('align-left', textIconIcon);
export const AlignRight = /* @__PURE__ */ makeIcon('align-right', textIconIcon);
export const Archive = /* @__PURE__ */ makeIcon('archive', mediaLibraryIcon);
export const ArrowDown = /* @__PURE__ */ makeIcon('arrow-down', arrowDownGoogleIcon);
export const ArrowLeft = /* @__PURE__ */ makeIcon('arrow-left', arrowLeftGoogleIcon);
export const ArrowRight = /* @__PURE__ */ makeIcon('arrow-right', arrowRightGoogleIcon);
export const ArrowRightLeft = /* @__PURE__ */ makeIcon('arrow-right-left', arrowBidirectionalHorizontalIcon);
export const ArrowUp = /* @__PURE__ */ makeIcon('arrow-up', arrowUpGoogleIcon);
export const ArrowUpCircle = /* @__PURE__ */ makeIcon('arrow-up-circle', arrowUpGoogleIcon);
export const ArrowUpDown = /* @__PURE__ */ makeIcon('arrow-up-down', arrowBidirectionalVerticalIcon);
export const Ban = /* @__PURE__ */ makeIcon('ban', notAllowedIcon);
export const BarChart2 = /* @__PURE__ */ makeIcon('bar-chart2', trendIcon);
export const BarChart3 = /* @__PURE__ */ makeIcon('bar-chart3', trendIcon);
export const Battery = /* @__PURE__ */ makeIcon('battery', batteryHorizontal100Icon);
export const Beaker = /* @__PURE__ */ makeIcon('beaker', simulationIcon);
export const Bell = /* @__PURE__ */ makeIcon('bell', notificationIcon);
export const BellOff = /* @__PURE__ */ makeIcon('bell-off', alertOffGoogleIcon);
export const Book = /* @__PURE__ */ makeIcon('book', logOpenGoogleIcon);
export const Bookmark = /* @__PURE__ */ makeIcon('bookmark', pinGoogleIcon);
export const Box = /* @__PURE__ */ makeIcon('box', tankIcon);
export const Briefcase = /* @__PURE__ */ makeIcon('briefcase', databaseIcon);
export const Bug = /* @__PURE__ */ makeIcon('bug', diagnosticGoogleIcon);
export const Building2 = /* @__PURE__ */ makeIcon('building2', homeIcon);
export const Cable = /* @__PURE__ */ makeIcon('cable', ethernetGoogleIcon);
export const Calculator = /* @__PURE__ */ makeIcon('calculator', tableIcon);
export const Calendar = /* @__PURE__ */ makeIcon('calendar', calendarGoogleIcon);
export const CalendarClock = /* @__PURE__ */ makeIcon('calendar-clock', calendarDateGoogleIcon);
export const CalendarDays = /* @__PURE__ */ makeIcon('calendar-days', calendarGoogleIcon);
export const Camera = /* @__PURE__ */ makeIcon('camera', cameraIcon);
export const CameraOff = /* @__PURE__ */ makeIcon('camera-off', cameraOffIcon);
export const Check = /* @__PURE__ */ makeIcon('check', checkGoogleIcon);
export const CheckCircle = /* @__PURE__ */ makeIcon('check-circle', completedIcon);
export const CheckCircle2 = /* @__PURE__ */ makeIcon('check-circle2', completedIcon);
export const CheckSquare = /* @__PURE__ */ makeIcon('check-square', checkboxCheckedIcon);
export const ChevronDown = /* @__PURE__ */ makeIcon('chevron-down', chevronDownGoogleIcon);
export const ChevronLeft = /* @__PURE__ */ makeIcon('chevron-left', chevronLeftGoogleIcon);
export const ChevronRight = /* @__PURE__ */ makeIcon('chevron-right', chevronRightGoogleIcon);
export const ChevronUp = /* @__PURE__ */ makeIcon('chevron-up', chevronUpGoogleIcon);
export const ChevronsLeft = /* @__PURE__ */ makeIcon('chevrons-left', chevronDoubleLeftGoogleIcon);
export const ChevronsRight = /* @__PURE__ */ makeIcon('chevrons-right', chevronDoubleRightGoogleIcon);
export const Circle = /* @__PURE__ */ makeIcon('circle', notStartedIcon);
export const CircleDot = /* @__PURE__ */ makeIcon('circle-dot', mediaRecordIcon);
export const ClipboardList = /* @__PURE__ */ makeIcon('clipboard-list', clipboardIcon);
export const Clock = /* @__PURE__ */ makeIcon('clock', timeIcon);
export const Clock3 = /* @__PURE__ */ makeIcon('clock3', timeIcon);
export const Cloud = /* @__PURE__ */ makeIcon('cloud', connection4BarsIcon);
export const CloudOff = /* @__PURE__ */ makeIcon('cloud-off', connectionOffIcon);
export const Code = /* @__PURE__ */ makeIcon('code', codeGoogleIcon);
export const Coffee = /* @__PURE__ */ makeIcon('coffee', foodGoogleIcon);
export const Cog = /* @__PURE__ */ makeIcon('cog', settingsIecIcon);
export const Coins = /* @__PURE__ */ makeIcon('coins', logEditGoogleIcon);
export const Columns = /* @__PURE__ */ makeIcon('columns', screenSplitLeftIcon);
export const Combine = /* @__PURE__ */ makeIcon('combine', widgetAddGoogleIcon);
export const Compass = /* @__PURE__ */ makeIcon('compass', conningIecIcon);
export const Copy = /* @__PURE__ */ makeIcon('copy', contentCopyGoogleIcon);
export const Cpu = /* @__PURE__ */ makeIcon('cpu', computerServerIcon);
export const CreditCard = /* @__PURE__ */ makeIcon('credit-card', logEditGoogleIcon);
export const Cylinder = /* @__PURE__ */ makeIcon('cylinder', tankIcon);
export const Database = /* @__PURE__ */ makeIcon('database', databaseIcon);
export const Disc3 = /* @__PURE__ */ makeIcon('disc3', databaseIcon);
export const DollarSign = /* @__PURE__ */ makeIcon('dollar-sign', logEditGoogleIcon);
export const DoorClosed = /* @__PURE__ */ makeIcon('door-closed', commandLockedIcon);
export const DoorOpen = /* @__PURE__ */ makeIcon('door-open', signExitIcon);
export const Download = /* @__PURE__ */ makeIcon('download', fileDownloadGoogleIcon);
export const Droplet = /* @__PURE__ */ makeIcon('droplet', sensorWaterDropGoogleIcon);
export const Droplets = /* @__PURE__ */ makeIcon('droplets', sensorWaterDropGoogleIcon);
export const Edit2 = /* @__PURE__ */ makeIcon('edit2', editGoogleIcon);
export const Edit3 = /* @__PURE__ */ makeIcon('edit3', editGoogleIcon);
export const Equal = /* @__PURE__ */ makeIcon('equal', equalIcon);
export const Eraser = /* @__PURE__ */ makeIcon('eraser', clearIcon);
export const ExternalLink = /* @__PURE__ */ makeIcon('external-link', applicationOpenGoogleIcon);
export const Eye = /* @__PURE__ */ makeIcon('eye', visibilityOnGoogleIcon);
export const EyeOff = /* @__PURE__ */ makeIcon('eye-off', visibilityOffGoogleIcon);
export const Fan = /* @__PURE__ */ makeIcon('fan', fanStaticIcon);
export const File = /* @__PURE__ */ makeIcon('file', logOpenGoogleIcon);
export const FileBox = /* @__PURE__ */ makeIcon('file-box', tankIcon);
export const FileCode = /* @__PURE__ */ makeIcon('file-code', codeGoogleIcon);
export const FileSpreadsheet = /* @__PURE__ */ makeIcon('file-spreadsheet', tableIcon);
export const FileText = /* @__PURE__ */ makeIcon('file-text', logOpenGoogleIcon);
export const FileVideo = /* @__PURE__ */ makeIcon('file-video', screenRecordingIcon);
export const FileWarning = /* @__PURE__ */ makeIcon('file-warning', warningGoogleIcon);
export const Film = /* @__PURE__ */ makeIcon('film', screenRecordingIcon);
export const Filter = /* @__PURE__ */ makeIcon('filter', filterIcon);
export const Flame = /* @__PURE__ */ makeIcon('flame', fireIcon);
export const Folder = /* @__PURE__ */ makeIcon('folder', logOpenGoogleIcon);
export const FolderArchive = /* @__PURE__ */ makeIcon('folder-archive', mediaLibraryIcon);
export const FolderInput = /* @__PURE__ */ makeIcon('folder-input', moveItemInIcon);
export const FolderKanban = /* @__PURE__ */ makeIcon('folder-kanban', widgetsIcon);
export const FolderOpen = /* @__PURE__ */ makeIcon('folder-open', logOpenGoogleIcon);
export const FolderPlus = /* @__PURE__ */ makeIcon('folder-plus', widgetAddGoogleIcon);
export const FolderSymlink = /* @__PURE__ */ makeIcon('folder-symlink', linkIcon);
export const FolderTree = /* @__PURE__ */ makeIcon('folder-tree', applicationsIcon);
export const Fullscreen = /* @__PURE__ */ makeIcon('fullscreen', screenFullGoogleIcon);
export const GanttChart = /* @__PURE__ */ makeIcon('gantt-chart', reorderHorizontalGoogleIcon);
export const Gauge = /* @__PURE__ */ makeIcon('gauge', speedIcon);
export const Gift = /* @__PURE__ */ makeIcon('gift', trophyIcon);
export const GitBranch = /* @__PURE__ */ makeIcon('git-branch', applicationsIcon);
export const GitCompare = /* @__PURE__ */ makeIcon('git-compare', compareGoogleIcon);
export const Github = /* @__PURE__ */ makeIcon('github', codeGoogleIcon);
export const Globe = /* @__PURE__ */ makeIcon('globe', ecdisIecIcon);
export const Grid2x2 = /* @__PURE__ */ makeIcon('grid2x2', screenQuadIcon);
export const GripVertical = /* @__PURE__ */ makeIcon('grip-vertical', dragVerticalIcon);
export const Group = /* @__PURE__ */ makeIcon('group', applicationsIcon);
export const Hand = /* @__PURE__ */ makeIcon('hand', touchIcon);
export const HardDrive = /* @__PURE__ */ makeIcon('hard-drive', databaseIcon);
export const Hash = /* @__PURE__ */ makeIcon('hash', codeGoogleIcon);
export const Headphones = /* @__PURE__ */ makeIcon('headphones', comPaIcon);
export const Heart = /* @__PURE__ */ makeIcon('heart', starSelectedIcon);
export const HeartPulse = /* @__PURE__ */ makeIcon('heart-pulse', monitoringIcon);
export const History = /* @__PURE__ */ makeIcon('history', historyGoogleIcon);
export const Home = /* @__PURE__ */ makeIcon('home', homeIcon);
export const Hourglass = /* @__PURE__ */ makeIcon('hourglass', pendingIcon);
export const Image = /* @__PURE__ */ makeIcon('image', imageIcon);
export const ImageOff = /* @__PURE__ */ makeIcon('image-off', imageIcon);
export const Images = /* @__PURE__ */ makeIcon('images', imageIcon);
export const Info = /* @__PURE__ */ makeIcon('info', infoIcon);
export const Key = /* @__PURE__ */ makeIcon('key', commandLockedIcon);
export const KeyRound = /* @__PURE__ */ makeIcon('key-round', commandLockedIcon);
export const Keyboard = /* @__PURE__ */ makeIcon('keyboard', inputKeyboardGoogleIcon);
export const Layers = /* @__PURE__ */ makeIcon('layers', chartLayersIcon);
export const LayoutGrid = /* @__PURE__ */ makeIcon('layout-grid', screenQuadIcon);
export const Library = /* @__PURE__ */ makeIcon('library', mediaLibraryIcon);
export const Lightbulb = /* @__PURE__ */ makeIcon('lightbulb', lightsIcon);
export const LineChart = /* @__PURE__ */ makeIcon('line-chart', trendIcon);
export const Link = /* @__PURE__ */ makeIcon('link', linkIcon);
export const Link2 = /* @__PURE__ */ makeIcon('link2', linkIcon);
export const List = /* @__PURE__ */ makeIcon('list', listAltCheckGoogleIcon);
export const ListOrdered = /* @__PURE__ */ makeIcon('list-ordered', listAltCheckGoogleIcon);
export const ListTodo = /* @__PURE__ */ makeIcon('list-todo', listAltCheckGoogleIcon);
export const Loader2 = /* @__PURE__ */ makeIcon('loader2', inProgress1Icon);
export const Lock = /* @__PURE__ */ makeIcon('lock', commandLockedIcon);
export const LockOpen = /* @__PURE__ */ makeIcon('lock-open', onIecIcon);
export const LogIn = /* @__PURE__ */ makeIcon('log-in', inputRightIcon);
export const LogOut = /* @__PURE__ */ makeIcon('log-out', signExitIcon);
export const Mail = /* @__PURE__ */ makeIcon('mail', comMessageGoogleIcon);
export const Map = /* @__PURE__ */ makeIcon('map', ecdisIecIcon);
export const MapPin = /* @__PURE__ */ makeIcon('map-pin', pinGoogleIcon);
export const Maximize = /* @__PURE__ */ makeIcon('maximize', screenFullGoogleIcon);
export const Maximize2 = /* @__PURE__ */ makeIcon('maximize2', screenFullGoogleIcon);
export const Megaphone = /* @__PURE__ */ makeIcon('megaphone', comPaIcon);
export const MemoryStick = /* @__PURE__ */ makeIcon('memory-stick', databaseIcon);
export const Menu = /* @__PURE__ */ makeIcon('menu', menuIcon);
export const Minimize = /* @__PURE__ */ makeIcon('minimize', screenFullExitGoogleIcon);
export const Minimize2 = /* @__PURE__ */ makeIcon('minimize2', screenFullExitGoogleIcon);
export const Minus = /* @__PURE__ */ makeIcon('minus', cursorRemovePointIconIcon);
export const MinusCircle = /* @__PURE__ */ makeIcon('minus-circle', cursorRemovePointIconIcon);
export const MinusSquare = /* @__PURE__ */ makeIcon('minus-square', cursorRemovePointIconIcon);
export const Monitor = /* @__PURE__ */ makeIcon('monitor', computerPcIcon);
export const MonitorPlay = /* @__PURE__ */ makeIcon('monitor-play', screenSharingIcon);
export const Moon = /* @__PURE__ */ makeIcon('moon', paletteNightIcon);
export const MoreHorizontal = /* @__PURE__ */ makeIcon('more-horizontal', moreGoogleIcon);
export const MoreVertical = /* @__PURE__ */ makeIcon('more-vertical', moreVerticalGoogleIcon);
export const Move = /* @__PURE__ */ makeIcon('move', transformMoveIcon);
export const MoveRight = /* @__PURE__ */ makeIcon('move-right', arrowRightGoogleIcon);
export const Music = /* @__PURE__ */ makeIcon('music', soundIcon);
export const Package = /* @__PURE__ */ makeIcon('package', tankIcon);
export const PackageCheck = /* @__PURE__ */ makeIcon('package-check', passedIcon);
export const PackageOpen = /* @__PURE__ */ makeIcon('package-open', logOpenGoogleIcon);
export const Palette = /* @__PURE__ */ makeIcon('palette', paletteColorCalibratedIecIcon);
export const PanelRightClose = /* @__PURE__ */ makeIcon('panel-right-close', panelRightCloseIcon);
export const PanelRightOpen = /* @__PURE__ */ makeIcon('panel-right-open', panelRightOpenIcon);
export const Pause = /* @__PURE__ */ makeIcon('pause', mediaPauseIcon);
export const Pencil = /* @__PURE__ */ makeIcon('pencil', editGoogleIcon);
export const Phone = /* @__PURE__ */ makeIcon('phone', comCallGoogleIcon);
export const PictureInPicture2 = /* @__PURE__ */ makeIcon('picture-in-picture2', screenSharingIcon);
export const Play = /* @__PURE__ */ makeIcon('play', mediaPlayIcon);
export const PlayCircle = /* @__PURE__ */ makeIcon('play-circle', mediaPlayIcon);
export const Plug = /* @__PURE__ */ makeIcon('plug', usbIcon);
export const Plus = /* @__PURE__ */ makeIcon('plus', widgetAddGoogleIcon);
export const Power = /* @__PURE__ */ makeIcon('power', onOffIecIcon);
export const PowerOff = /* @__PURE__ */ makeIcon('power-off', offIecIcon);
export const Printer = /* @__PURE__ */ makeIcon('printer', printIcon);
export const QrCode = /* @__PURE__ */ makeIcon('qr-code', screenShotIcon);
export const Radio = /* @__PURE__ */ makeIcon('radio', comRadioIcon);
export const Receipt = /* @__PURE__ */ makeIcon('receipt', logEditGoogleIcon);
export const RectangleHorizontal = /* @__PURE__ */ makeIcon('rectangle-horizontal', screenPadIcon);
export const RefreshCw = /* @__PURE__ */ makeIcon('refresh-cw', refreshGoogleIcon);
export const Repeat = /* @__PURE__ */ makeIcon('repeat', mediaLoopIcon);
export const RotateCcw = /* @__PURE__ */ makeIcon('rotate-ccw', undoIecIcon);
export const RotateCw = /* @__PURE__ */ makeIcon('rotate-cw', redoIecIcon);
export const Ruler = /* @__PURE__ */ makeIcon('ruler', measureGoogleIcon);
export const Save = /* @__PURE__ */ makeIcon('save', saveProposalIcon);
export const Scale = /* @__PURE__ */ makeIcon('scale', limitsInsideIcon);
export const ScanEye = /* @__PURE__ */ makeIcon('scan-eye', screenShotIcon);
export const ScanSearch = /* @__PURE__ */ makeIcon('scan-search', searchIcon);
export const Scissors = /* @__PURE__ */ makeIcon('scissors', contentCutGoogleIcon);
export const Search = /* @__PURE__ */ makeIcon('search', searchIcon);
export const Send = /* @__PURE__ */ makeIcon('send', sendIcon);
export const Server = /* @__PURE__ */ makeIcon('server', computerServerIcon);
export const ServerCog = /* @__PURE__ */ makeIcon('server-cog', settingsDefaultIecIcon);
export const Settings = /* @__PURE__ */ makeIcon('settings', settingsIecIcon);
export const Settings2 = /* @__PURE__ */ makeIcon('settings2', configureIcon);
export const Shield = /* @__PURE__ */ makeIcon('shield', commandLockedIcon);
export const ShieldAlert = /* @__PURE__ */ makeIcon('shield-alert', warningGoogleIcon);
export const ShieldCheck = /* @__PURE__ */ makeIcon('shield-check', passedFillIcon);
export const ShieldOff = /* @__PURE__ */ makeIcon('shield-off', notAllowedIcon);
export const ShoppingCart = /* @__PURE__ */ makeIcon('shopping-cart', attachmentIcon);
export const Signal = /* @__PURE__ */ makeIcon('signal', cellFullIcon);
export const SkipBack = /* @__PURE__ */ makeIcon('skip-back', mediaSkipPreviousIcon);
export const SkipForward = /* @__PURE__ */ makeIcon('skip-forward', mediaSkipNextIcon);
export const Sliders = /* @__PURE__ */ makeIcon('sliders', configureIcon);
export const SlidersHorizontal = /* @__PURE__ */ makeIcon('sliders-horizontal', configureIcon);
export const Smartphone = /* @__PURE__ */ makeIcon('smartphone', screenPadIcon);
export const Snail = /* @__PURE__ */ makeIcon('snail', speedLowIcon);
export const Snowflake = /* @__PURE__ */ makeIcon('snowflake', tempColdIcon);
export const SortAsc = /* @__PURE__ */ makeIcon('sort-asc', sortGoogleIcon);
export const SortDesc = /* @__PURE__ */ makeIcon('sort-desc', sortGoogleIcon);
export const Sparkles = /* @__PURE__ */ makeIcon('sparkles', simulationIcon);
export const Square = /* @__PURE__ */ makeIcon('square', checkboxUncheckGoogleIcon);
export const Star = /* @__PURE__ */ makeIcon('star', starIcon);
export const Stethoscope = /* @__PURE__ */ makeIcon('stethoscope', diagnosticGoogleIcon);
export const StickyNote = /* @__PURE__ */ makeIcon('sticky-note', logOpenGoogleIcon);
export const StopCircle = /* @__PURE__ */ makeIcon('stop-circle', mediaStopIcon);
export const Store = /* @__PURE__ */ makeIcon('store', databaseIcon);
export const Sun = /* @__PURE__ */ makeIcon('sun', paletteDayIcon);
export const TableProperties = /* @__PURE__ */ makeIcon('table-properties', tableIcon);
export const Tag = /* @__PURE__ */ makeIcon('tag', licenseGoogleIcon);
export const Tags = /* @__PURE__ */ makeIcon('tags', licenseGoogleIcon);
export const Target = /* @__PURE__ */ makeIcon('target', sensorGpsFullIcon);
export const Terminal = /* @__PURE__ */ makeIcon('terminal', codeGoogleIcon);
export const Thermometer = /* @__PURE__ */ makeIcon('thermometer', tempHotcoldIcon);
export const ThumbsDown = /* @__PURE__ */ makeIcon('thumbs-down', notAllowedIcon);
export const ThumbsUp = /* @__PURE__ */ makeIcon('thumbs-up', passedIcon);
export const Timer = /* @__PURE__ */ makeIcon('timer', timerGoogleIcon);
export const Trash2 = /* @__PURE__ */ makeIcon('trash2', deleteIcon);
export const TrendingDown = /* @__PURE__ */ makeIcon('trending-down', trendDownIcon);
export const TrendingUp = /* @__PURE__ */ makeIcon('trending-up', trendUpIcon);
export const Ungroup = /* @__PURE__ */ makeIcon('ungroup', applicationsIcon);
export const Unlink = /* @__PURE__ */ makeIcon('unlink', linkRemoveIcon);
export const Unlock = /* @__PURE__ */ makeIcon('unlock', onIecIcon);
export const Upload = /* @__PURE__ */ makeIcon('upload', fileUploadGoogleIcon);
export const UploadCloud = /* @__PURE__ */ makeIcon('upload-cloud', fileUploadGoogleIcon);
export const User = /* @__PURE__ */ makeIcon('user', userGoogleIcon);
export const UserMinus = /* @__PURE__ */ makeIcon('user-minus', userProfilesIcon);
export const Users = /* @__PURE__ */ makeIcon('users', userProfilesIcon);
export const Video = /* @__PURE__ */ makeIcon('video', screenRecordingIcon);
export const VideoOff = /* @__PURE__ */ makeIcon('video-off', cameraOffIcon);
export const Volume2 = /* @__PURE__ */ makeIcon('volume2', soundIcon);
export const VolumeX = /* @__PURE__ */ makeIcon('volume-x', soundMutedIcon);
export const Wallet = /* @__PURE__ */ makeIcon('wallet', databaseIcon);
export const Wand2 = /* @__PURE__ */ makeIcon('wand2', simulationIcon);
export const Weight = /* @__PURE__ */ makeIcon('weight', limitsInsideIcon);
export const Wifi = /* @__PURE__ */ makeIcon('wifi', wifiStrength4GoogleIcon);
export const WifiOff = /* @__PURE__ */ makeIcon('wifi-off', wifiOffGoogleIcon);
export const Wind = /* @__PURE__ */ makeIcon('wind', windIcon);
export const Workflow = /* @__PURE__ */ makeIcon('workflow', applicationsIcon);
export const Wrench = /* @__PURE__ */ makeIcon('wrench', wrenchIcon);
export const X = /* @__PURE__ */ makeIcon('x', closeGoogleIcon);
export const XCircle = /* @__PURE__ */ makeIcon('xcircle', errorIcon);
export const Zap = /* @__PURE__ */ makeIcon('zap', boltGoogleIcon);
export const ZoomIn = /* @__PURE__ */ makeIcon('zoom-in', contentExpandGoogleIcon);
export const ZoomOut = /* @__PURE__ */ makeIcon('zoom-out', contentCollapseGoogleIcon);
export function Bold({className = ''}: IconProps) {
  return <span aria-hidden="true" className={className} style={{fontWeight: 700 }}>B</span>;
}
export function Italic({className = ''}: IconProps) {
  return <span aria-hidden="true" className={className} style={{fontStyle: 'italic' }}>I</span>;
}
export function Underline({className = ''}: IconProps) {
  return <span aria-hidden="true" className={className} style={{textDecoration: 'underline' }}>U</span>;
}
