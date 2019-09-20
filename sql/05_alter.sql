use `isucari`


update `items` set `timedateid` = concat(date_format(created_at, '%Y%m%d%H%i%S'), lpad(id, 8, '0'));
 alter table items add unique timedateid (timedateid);
