// The named rooms for a page that shows or picks rooms (see rooms.ts). Call it while a component is built.
import { t } from './i18n.svelte'
import { resource } from './resource.svelte'
import { roomItems, roomName, channelLabel, serverLabel, type RoomItem, type RoomWords } from './rooms'

export const roomWords = (): RoomWords => ({
  hidden: (tail) => t('rp.hidden', { tail }),
  server: (tail) => t('rp.server', { tail }),
  kakao: t('rp.kakao'),
})

export function useRooms() {
  const blocks = resource<any>('memory.blocks.list', () => ({}), { live: ['memory', 'config'] })
  const channels = resource<any>('channels.list', () => ({}), { live: ['config', 'discord'] })
  const items = $derived<RoomItem[]>(roomItems(blocks.data?.blocks, channels.data, roomWords()))
  return {
    get items() {
      return items
    },
    get ready() {
      return !!blocks.data
    },
    /** a room key (a search hit's source) as a name and its server */
    name: (key: string) => roomName(key, items, roomWords()),
    /** one Discord channel by the names Discord gave */
    channel: (name: string | undefined, id: string) => channelLabel(name, id, roomWords()),
    server: (name: string | undefined, id: string) => serverLabel(name, id, roomWords()),
  }
}
